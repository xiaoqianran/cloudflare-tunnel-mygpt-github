package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func validReleaseRef(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
		return false
	}
	return len(value) <= 200
}

func (s *Server) ghEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "GH_PROMPT_DISABLED=1")
	if s.cfg.GitHubToken != "" {
		env = append(env, "GH_TOKEN="+s.cfg.GitHubToken)
	}
	return env
}

func (s *Server) gh(ctx context.Context, stdin string, args ...string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = s.ghEnv()
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("gh command timed out after %s", s.cfg.CommandTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo       string `json:"repo"`
		Tag        string `json:"tag"`
		Title      string `json:"title"`
		Notes      string `json:"notes"`
		Target     string `json:"target"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tag := strings.TrimSpace(input.Tag)
	if !validReleaseRef(tag) {
		writeError(w, 400, "invalid release tag")
		return
	}
	target := strings.TrimSpace(input.Target)
	if target != "" && !validReleaseRef(target) {
		writeError(w, 400, "invalid release target")
		return
	}
	title := strings.TrimSpace(input.Title)
	if len(title) > 256 {
		writeError(w, 400, "release title must be at most 256 characters")
		return
	}
	if len(input.Notes) > 500_000 {
		writeError(w, 400, "release notes must be at most 500000 bytes")
		return
	}

	unlock := s.locker.Lock(repo)
	defer unlock()

	args := []string{"release", "create", tag, "--repo", repo, "--notes-file", "-"}
	if title != "" {
		args = append(args, "--title", title)
	}
	if target != "" {
		args = append(args, "--target", target)
	}
	if input.Draft {
		args = append(args, "--draft")
	}
	if input.Prerelease {
		args = append(args, "--prerelease")
	}

	result, err := s.gh(r.Context(), input.Notes, args...)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			writeError(w, 503, "GitHub CLI (gh) is not installed on the host")
			return
		}
		writeError(w, 502, err.Error())
		return
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		if message == "" {
			message = fmt.Sprintf("gh release create failed with exit code %d", result.ExitCode)
		}
		writeError(w, 502, message)
		return
	}

	writeJSON(w, 200, map[string]any{
		"repo":       repo,
		"tag":        tag,
		"title":      title,
		"target":     target,
		"draft":      input.Draft,
		"prerelease": input.Prerelease,
		"url":        strings.TrimSpace(result.Stdout),
	})
}
