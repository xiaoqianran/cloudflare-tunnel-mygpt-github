package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type commandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (s *Server) gitEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if s.cfg.GitRemoteToken != "" {
		credential := base64.StdEncoding.EncodeToString([]byte(s.cfg.GitRemoteUsername + ":" + s.cfg.GitRemoteToken))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+credential,
		)
	}
	return env
}

func (s *Server) runCommand(ctx context.Context, dir, name string, args ...string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = s.gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
	if err == nil {
		return result, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("command timed out after %s", s.cfg.CommandTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func (s *Server) git(ctx context.Context, dir string, args ...string) (commandResult, error) {
	return s.runCommand(ctx, dir, "git", args...)
}

func (s *Server) requireGitOK(ctx context.Context, dir string, args ...string) (commandResult, error) {
	result, err := s.git(ctx, dir, args...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		if message == "" {
			message = fmt.Sprintf("git %s failed with exit code %d", strings.Join(args, " "), result.ExitCode)
		}
		return result, errors.New(message)
	}
	return result, nil
}

func (s *Server) remoteURL(repo string) string {
	return strings.TrimRight(s.cfg.GitRemoteBaseURL, "/") + "/" + repo + ".git"
}

func (s *Server) repoExists(repo string) bool {
	path := repoDir(s.cfg.WorkspaceRoot, repo)
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

func (s *Server) ensureRepo(repo string) (string, error) {
	path := repoDir(s.cfg.WorkspaceRoot, repo)
	if !s.repoExists(repo) {
		return "", fmt.Errorf("repository is not cloned locally; call syncRepository first")
	}
	return path, nil
}

func (s *Server) cloneRepo(ctx context.Context, repo string) (string, error) {
	path := repoDir(s.cfg.WorkspaceRoot, repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	result, err := s.git(ctx, s.cfg.WorkspaceRoot, "clone", "--", s.remoteURL(repo), path)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		_ = os.RemoveAll(path)
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = "git clone failed"
		}
		return "", errors.New(message)
	}
	return path, nil
}

func (s *Server) listRepoFiles(ctx context.Context, dir string) ([]string, error) {
	result, err := s.requireGitOK(ctx, dir, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(result.Stdout, "\x00")
	files := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = filepath.ToSlash(strings.TrimSpace(part))
		if part == "" || strings.HasPrefix(part, ".git/") || part == ".git" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		files = append(files, part)
	}
	return files, nil
}

func (s *Server) repoState(ctx context.Context, dir string) (map[string]any, error) {
	head, err := s.requireGitOK(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	branch, _ := s.git(ctx, dir, "branch", "--show-current")
	status, err := s.requireGitOK(ctx, dir, "status", "--porcelain=v1")
	if err != nil {
		return nil, err
	}
	remote, _ := s.git(ctx, dir, "remote", "get-url", "origin")
	return map[string]any{
		"head_sha": strings.TrimSpace(head.Stdout),
		"branch":   strings.TrimSpace(branch.Stdout),
		"dirty":    strings.TrimSpace(status.Stdout) != "",
		"remote":   strings.TrimSpace(remote.Stdout),
	}, nil
}
