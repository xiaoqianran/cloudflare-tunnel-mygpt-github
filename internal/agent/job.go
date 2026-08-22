package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	jobRetention        = 24 * time.Hour
	defaultJobWait      = 10 * time.Second
	maxJobWait          = 20 * time.Second
	defaultJobTailChars = 12000
)

type commandInput struct {
	Command        string `json:"command"`
	Workdir        string `json:"workdir"`
	Stdin          string `json:"stdin"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type commandJob struct {
	ID         string
	Status     string
	Workdir    string
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
	Revision   uint64
	result     *hostCommandResult
	stdout     *cappedBuffer
	stderr     *cappedBuffer
	changed    chan struct{}
	cancel     context.CancelFunc
}

type commandJobView struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Workdir    string     `json:"workdir"`
	Revision   uint64     `json:"revision"`
	ExitCode   *int       `json:"exit_code"`
	Stdout     string     `json:"stdout"`
	Stderr     string     `json:"stderr"`
	TimedOut   bool       `json:"timed_out"`
	Truncated  bool       `json:"truncated"`
	DurationMS int64      `json:"duration_ms"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type jobStore struct {
	mu   sync.RWMutex
	jobs map[string]*commandJob
}

func newJobStore() *jobStore { return &jobStore{jobs: make(map[string]*commandJob)} }

func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) startJob(input commandInput) (string, error) {
	id, err := newJobID()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	workdir := strings.TrimSpace(input.Workdir)
	if workdir == "" {
		workdir = "/root"
	}
	job := &commandJob{
		ID:        id,
		Status:    "running",
		Workdir:   workdir,
		StartedAt: time.Now().UTC(),
		Revision:  1,
		changed:   make(chan struct{}),
		cancel:    cancel,
	}
	job.stdout = s.newCappedBuffer()
	job.stderr = s.newCappedBuffer()
	job.stdout.onWrite = func() { s.touchJob(id) }
	job.stderr.onWrite = func() { s.touchJob(id) }

	s.jobs.mu.Lock()
	s.jobs.pruneLocked(job.StartedAt)
	s.jobs.jobs[id] = job
	s.jobs.mu.Unlock()

	go func() {
		result, err := s.runHostCommandTo(ctx, input.Command, input.Workdir, input.Stdin, input.TimeoutSeconds, job.stdout, job.stderr)
		finished := time.Now().UTC()

		s.jobs.mu.Lock()
		defer s.jobs.mu.Unlock()
		job.FinishedAt = &finished
		job.cancel = nil
		if err != nil {
			job.Status = "failed"
			job.Error = err.Error()
			s.signalJobLocked(job)
			return
		}
		job.result = &result
		job.Workdir = result.Workdir
		switch {
		case ctx.Err() == context.Canceled:
			job.Status = "cancelled"
		case result.TimedOut:
			job.Status = "timed_out"
		default:
			job.Status = "completed"
		}
		s.signalJobLocked(job)
	}()
	return id, nil
}

func (s *Server) touchJob(id string) {
	s.jobs.mu.Lock()
	if job := s.jobs.jobs[id]; job != nil && job.Status == "running" {
		s.signalJobLocked(job)
	}
	s.jobs.mu.Unlock()
}

func (s *Server) signalJobLocked(job *commandJob) {
	job.Revision++
	close(job.changed)
	job.changed = make(chan struct{})
}

func (j *jobStore) pruneLocked(now time.Time) {
	for id, job := range j.jobs {
		if job.FinishedAt != nil && now.Sub(*job.FinishedAt) > jobRetention {
			delete(j.jobs, id)
		}
	}
}

func (s *Server) getJob(id string, tailChars int) (commandJobView, <-chan struct{}, bool) {
	s.jobs.mu.RLock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		s.jobs.mu.RUnlock()
		return commandJobView{}, nil, false
	}
	view := commandJobView{
		ID:         job.ID,
		Status:     job.Status,
		Workdir:    job.Workdir,
		Revision:   job.Revision,
		Error:      job.Error,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
	result := job.result
	stdout := job.stdout
	stderr := job.stderr
	changed := job.changed
	s.jobs.mu.RUnlock()

	view.Stdout, view.Truncated = stdout.tail(tailChars)
	stderrText, stderrTruncated := stderr.tail(tailChars)
	view.Stderr = stderrText
	view.Truncated = view.Truncated || stderrTruncated
	if result != nil {
		exitCode := result.ExitCode
		view.ExitCode = &exitCode
		view.TimedOut = result.TimedOut
		view.DurationMS = result.DurationMS
		view.Truncated = view.Truncated || result.Truncated
	} else {
		view.DurationMS = time.Since(view.StartedAt).Milliseconds()
	}
	return view, changed, true
}

func (s *Server) cancelJob(id string) (string, bool) {
	s.jobs.mu.RLock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		s.jobs.mu.RUnlock()
		return "", false
	}
	cancel := job.cancel
	s.jobs.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return id, true
}

func validateCommandInput(input commandInput) string {
	if strings.TrimSpace(input.Command) == "" {
		return "command is required"
	}
	if input.TimeoutSeconds < 0 {
		return "timeout_seconds must be positive"
	}
	return ""
}

func parseJobQuery(r *http.Request, maxTail int) (*uint64, time.Duration, int, error) {
	q := r.URL.Query()
	tailChars := 0

	var after *uint64
	if raw := q.Get("after"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("after must be a non-negative integer")
		}
		after = &value
		tailChars = defaultJobTailChars
	}

	wait := time.Duration(0)
	if raw := q.Get("wait_seconds"); raw != "" {
		if after == nil {
			return nil, 0, 0, fmt.Errorf("wait_seconds requires after")
		}
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 || time.Duration(seconds)*time.Second > maxJobWait {
			return nil, 0, 0, fmt.Errorf("wait_seconds must be between 0 and %d", int(maxJobWait/time.Second))
		}
		wait = time.Duration(seconds) * time.Second
	} else if after != nil {
		wait = defaultJobWait
	}

	if raw := q.Get("tail_chars"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > maxTail {
			return nil, 0, 0, fmt.Errorf("tail_chars must be between 1 and %d", maxTail)
		}
		tailChars = value
	}
	return after, wait, tailChars, nil
}

func (s *Server) handleStartCommand(w http.ResponseWriter, r *http.Request) {
	var input commandInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if message := validateCommandInput(input); message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	id, err := s.startJob(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create job")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": "running"})
}

func (s *Server) handleGetCommandJob(w http.ResponseWriter, r *http.Request) {
	maxTail := s.cfg.MaxCommandOutputChars
	if maxTail < 1000 {
		maxTail = 180000
	}
	after, wait, tailChars, err := parseJobQuery(r, maxTail)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var timeout <-chan time.Time
	var timer *time.Timer
	if wait > 0 {
		timer = time.NewTimer(wait)
		timeout = timer.C
		defer timer.Stop()
	}

	for {
		job, changed, ok := s.getJob(r.PathValue("id"), tailChars)
		if !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if after == nil || job.Status != "running" || job.Revision != *after || wait == 0 {
			writeJSON(w, http.StatusOK, job)
			return
		}

		select {
		case <-changed:
			continue
		case <-timeout:
			job, _, _ = s.getJob(r.PathValue("id"), tailChars)
			writeJSON(w, http.StatusOK, job)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleCancelCommandJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cancelJob(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}
