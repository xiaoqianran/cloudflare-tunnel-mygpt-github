package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			w.truncated = true
		}
		_, _ = w.buf.Write(p)
	} else if len(p) > 0 {
		w.truncated = true
	}
	return original, nil
}

func (w *cappedBuffer) String() string { return w.buf.String() }

func sandboxContainerName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return fmt.Sprintf("mygpt-%x", sum[:8])
}

func (s *Server) containerEngine() (string, error) {
	configured := strings.TrimSpace(s.cfg.CommandEngine)
	if configured != "" && configured != "auto" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("configured command sandbox engine %q is not installed", configured)
		}
		return path, nil
	}
	for _, candidate := range []string{"podman", "docker"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no command sandbox engine found; install podman or docker")
}

func runProcess(ctx context.Context, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, err
}

func (s *Server) ensureSandbox(ctx context.Context, repo, repoPath string) (string, string, error) {
	engine, err := s.containerEngine()
	if err != nil {
		return "", "", err
	}
	name := sandboxContainerName(repo)

	_, _, inspectCode, inspectErr := runProcess(ctx, engine, "inspect", name)
	if inspectErr != nil {
		return "", "", inspectErr
	}
	if inspectCode != 0 {
		if _, stderr, code, err := runProcess(ctx, engine, "pull", s.cfg.CommandImage); err != nil || code != 0 {
			if err != nil {
				return "", "", err
			}
			return "", "", fmt.Errorf("pull sandbox image: %s", strings.TrimSpace(stderr))
		}
		mount := repoPath + ":/workspace:rw"
		args := []string{
			"create",
			"--name", name,
			"--workdir", "/workspace",
			"--volume", mount,
			"--env", "HOME=/root",
			"--restart", "unless-stopped",
			s.cfg.CommandImage,
			"sleep", "infinity",
		}
		if _, stderr, code, err := runProcess(ctx, engine, args...); err != nil || code != 0 {
			if err != nil {
				return "", "", err
			}
			return "", "", fmt.Errorf("create sandbox: %s", strings.TrimSpace(stderr))
		}
	}

	stdout, stderr, code, err := runProcess(ctx, engine, "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return "", "", err
	}
	if code != 0 {
		return "", "", fmt.Errorf("inspect sandbox state: %s", strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) != "true" {
		if _, stderr, code, err := runProcess(ctx, engine, "start", name); err != nil || code != 0 {
			if err != nil {
				return "", "", err
			}
			return "", "", fmt.Errorf("start sandbox: %s", strings.TrimSpace(stderr))
		}
	}
	return engine, name, nil
}

type sandboxCommandResult struct {
	Repo       string `json:"repo"`
	Engine     string `json:"engine"`
	Container  string `json:"container"`
	Image      string `json:"image"`
	Workdir    string `json:"workdir"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

func (s *Server) runSandboxCommand(ctx context.Context, repo, command, workdir string, timeoutSeconds int) (sandboxCommandResult, error) {
	dir, err := s.ensureRepo(repo)
	if err != nil {
		return sandboxCommandResult{}, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return sandboxCommandResult{}, fmt.Errorf("command is required")
	}

	containerWorkdir := "/workspace"
	if strings.TrimSpace(workdir) != "" {
		clean := filepath.Clean(strings.TrimSpace(workdir))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return sandboxCommandResult{}, fmt.Errorf("workdir must be relative to the repository")
		}
		hostPath := filepath.Join(dir, clean)
		if rel, err := filepath.Rel(dir, hostPath); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return sandboxCommandResult{}, fmt.Errorf("workdir escapes repository")
		}
		containerWorkdir = "/workspace/" + filepath.ToSlash(clean)
	}

	timeout := s.cfg.CommandTimeout
	if timeoutSeconds > 0 {
		requested := time.Duration(timeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	engine, container, err := s.ensureSandbox(commandCtx, repo, dir)
	if err != nil {
		return sandboxCommandResult{}, err
	}

	args := []string{"exec", "--workdir", containerWorkdir, container, "/bin/bash", "-lc", command}
	cmd := exec.CommandContext(commandCtx, engine, args...)
	stdout := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	stderr := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started := time.Now()
	err = cmd.Run()
	duration := time.Since(started)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if commandCtx.Err() == nil {
			return sandboxCommandResult{}, err
		} else {
			exitCode = -1
		}
	}

	return sandboxCommandResult{
		Repo:       repo,
		Engine:     filepath.Base(engine),
		Container:  container,
		Image:      s.cfg.CommandImage,
		Workdir:    containerWorkdir,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		TimedOut:   commandCtx.Err() == context.DeadlineExceeded,
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: duration.Milliseconds(),
	}, nil
}
