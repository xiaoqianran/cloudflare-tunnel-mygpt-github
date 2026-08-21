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

func (s *Server) runHostCommand(ctx context.Context, command, workdir string, timeoutSeconds int) (hostCommandResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return hostCommandResult{}, fmt.Errorf("command is required")
	}

	dir := strings.TrimSpace(workdir)
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return hostCommandResult{}, err
		}
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
	if timeoutSeconds > 0 {
		requested := time.Duration(timeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "/bin/bash", "-lc", command)
	cmd.Dir = dir
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
