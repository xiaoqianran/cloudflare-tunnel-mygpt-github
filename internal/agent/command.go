package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const commonPath = "/root/.local/bin:/root/.cargo/bin:/root/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

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
	Workdir    string `json:"workdir"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

func commandEnv() []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	values["HOME"] = "/root"
	values["USER"] = "root"
	values["LOGNAME"] = "root"
	values["SHELL"] = "/bin/bash"
	if current := strings.TrimSpace(values["PATH"]); current != "" {
		values["PATH"] = commonPath + ":" + current
	} else {
		values["PATH"] = commonPath
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func (s *Server) runHostCommand(ctx context.Context, command, workdir, stdin string, timeoutSeconds int) (hostCommandResult, error) {
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

	// A login shell may rebuild PATH from root profile files. Prepend common
	// package-manager install locations inside the shell as well, while retaining
	// anything supplied by the host environment/profile.
	shellCommand := "export PATH=" + commonPath + ":\"$PATH\"; " + command
	cmd := exec.Command("/bin/bash", "-lc", shellCommand)
	cmd.Dir = dir
	cmd.Env = commandEnv()
	cmd.Stdin = strings.NewReader(stdin)
	// Put the shell and its descendants in their own process group so a timeout
	// terminates the whole workflow instead of leaving grandchildren behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	stderr := &cappedBuffer{limit: s.cfg.MaxCommandOutputChars}
	if stdout.limit < 1000 {
		stdout.limit = 180000
		stderr.limit = 180000
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return hostCommandResult{}, err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var runErr error
	timedOut := false
	select {
	case runErr = <-waitCh:
	case <-commandCtx.Done():
		timedOut = errors.Is(commandCtx.Err(), context.DeadlineExceeded)
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		runErr = <-waitCh
	}
	duration := time.Since(started)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	if commandCtx.Err() != nil && exitCode == 0 {
		exitCode = -1
	}

	return hostCommandResult{
		Workdir:    dir,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		TimedOut:   timedOut,
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: duration.Milliseconds(),
	}, nil
}
