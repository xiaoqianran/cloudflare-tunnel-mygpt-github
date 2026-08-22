package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
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

type hostCommandResult struct {
	Command    string `json:"command"`
	Workdir    string `json:"workdir"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

func commandEnv() []string {
	env := append([]string{}, os.Environ()...)
	// A root systemd service does not always inherit the same environment as an
	// interactive root login. Pin the basic identity expected by common CLIs.
	env = append(env,
		"HOME=/root",
		"USER=root",
		"LOGNAME=root",
		"SHELL=/bin/bash",
	)

	// gh prefers GH_TOKEN. Reuse a server-side GITHUB_TOKEN when one is already
	// configured, without requiring the GPT to transmit GitHub credentials.
	if strings.TrimSpace(os.Getenv("GH_TOKEN")) == "" {
		if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
			env = append(env, "GH_TOKEN="+token)
		}
	}
	return env
}

func (s *Server) runHostCommand(ctx context.Context, command, workdir string, timeoutSeconds int) (hostCommandResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return hostCommandResult{}, fmt.Errorf("command is required")
	}

	dir := strings.TrimSpace(workdir)
	if dir == "" {
		dir = "/root"
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return hostCommandResult{}, fmt.Errorf("invalid workdir: %w", err)
		}
		dir = abs
	}
	info, err := os.Stat(dir)
	if err != nil {
		return hostCommandResult{}, fmt.Errorf("workdir is unavailable: %w", err)
	}
	if !info.IsDir() {
		return hostCommandResult{}, fmt.Errorf("workdir is not a directory: %s", dir)
	}

	timeout := s.cfg.CommandTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	if timeoutSeconds > 0 {
		requested := time.Duration(timeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// bash -l may rebuild PATH from root profile files, so prepend the common
	// locations inside the executed shell as well as in the systemd unit. This
	// covers Go, pip/uv-installed CLIs, Cargo, snap and normal system binaries.
	const pathPrefix = "/root/.local/bin:/root/.cargo/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"
	shellCommand := "export PATH=" + pathPrefix + ":\"$PATH\"; " + command
	cmd := exec.CommandContext(commandCtx, "/bin/bash", "-lc", shellCommand)
	cmd.Dir = dir
	cmd.Env = commandEnv()
	stdout := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	stderr := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	if stdout.limit < 1000 {
		stdout.limit = 180000
		stderr.limit = 180000
	}
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
			return hostCommandResult{}, err
		} else {
			exitCode = -1
		}
	}

	return hostCommandResult{
		Command:    command,
		Workdir:    dir,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		TimedOut:   commandCtx.Err() == context.DeadlineExceeded,
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: duration.Milliseconds(),
	}, nil
}
