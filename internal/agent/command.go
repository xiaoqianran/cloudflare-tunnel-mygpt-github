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
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const commonPath = "/root/.local/bin:/root/.cargo/bin:/root/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

type cappedBuffer struct {
	mu        sync.RWMutex
	buf       bytes.Buffer
	limit     int
	truncated bool
	onWrite   func()
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}

	w.mu.Lock()
	if len(p) >= w.limit {
		w.buf.Reset()
		_, _ = w.buf.Write(p[len(p)-w.limit:])
		w.truncated = true
	} else {
		if overflow := w.buf.Len() + len(p) - w.limit; overflow > 0 {
			b := w.buf.Bytes()
			copy(b, b[overflow:])
			w.buf.Truncate(len(b) - overflow)
			w.truncated = true
		}
		_, _ = w.buf.Write(p)
	}
	onWrite := w.onWrite
	w.mu.Unlock()

	if onWrite != nil {
		onWrite()
	}
	return n, nil
}

func (w *cappedBuffer) snapshot() (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.buf.String(), w.truncated
}

func (w *cappedBuffer) tail(chars int) (string, bool) {
	text, truncated := w.snapshot()
	if chars <= 0 {
		return text, truncated
	}

	cut := len(text)
	for i := 0; i < chars && cut > 0; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:cut])
		cut -= size
	}
	if cut > 0 {
		return text[cut:], true
	}
	return text, truncated
}

func (w *cappedBuffer) String() string {
	text, _ := w.snapshot()
	return text
}

type hostCommandResult struct {
	Workdir    string `json:"workdir"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	cancelled  bool
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

func (s *Server) commandTimeoutLimit() time.Duration {
	timeout := s.cfg.CommandTimeout
	if timeout <= 0 {
		return 30 * time.Minute
	}
	return timeout
}

func (s *Server) runHostCommand(ctx context.Context, command, workdir, stdin string, timeoutSeconds int) (hostCommandResult, error) {
	return s.runHostCommandTo(ctx, command, workdir, stdin, timeoutSeconds, nil, nil)
}

func (s *Server) newCappedBuffer() *cappedBuffer {
	limit := s.cfg.MaxCommandOutputChars
	if limit < 1000 {
		limit = 180000
	}
	return &cappedBuffer{limit: limit}
}

func (s *Server) runHostCommandTo(ctx context.Context, command, workdir, stdin string, timeoutSeconds int, stdout, stderr *cappedBuffer) (hostCommandResult, error) {
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

	timeout := s.commandTimeoutLimit()
	if timeoutSeconds > 0 {
		requested := time.Duration(timeoutSeconds) * time.Second
		if requested > timeout {
			return hostCommandResult{}, fmt.Errorf("timeout_seconds exceeds server limit of %s", timeout)
		}
		timeout = requested
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

	if stdout == nil {
		stdout = s.newCappedBuffer()
	}
	if stderr == nil {
		stderr = s.newCappedBuffer()
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
	cancelled := false
	select {
	case runErr = <-waitCh:
	case <-commandCtx.Done():
		timedOut = errors.Is(commandCtx.Err(), context.DeadlineExceeded)
		cancelled = errors.Is(commandCtx.Err(), context.Canceled)
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
	if (timedOut || cancelled) && exitCode == 0 {
		exitCode = -1
	}

	stdoutText, stdoutTruncated := stdout.snapshot()
	stderrText, stderrTruncated := stderr.snapshot()
	return hostCommandResult{
		Workdir:    dir,
		ExitCode:   exitCode,
		Stdout:     stdoutText,
		Stderr:     stderrText,
		TimedOut:   timedOut,
		Truncated:  stdoutTruncated || stderrTruncated,
		DurationMS: duration.Milliseconds(),
		cancelled:  cancelled,
	}, nil
}
