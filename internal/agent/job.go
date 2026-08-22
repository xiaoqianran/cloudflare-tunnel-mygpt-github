package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const jobRetention = 24 * time.Hour

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
	result     *hostCommandResult
	stdout     *cappedBuffer
	stderr     *cappedBuffer
	cancel     context.CancelFunc
}

type commandJobView struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Workdir    string     `json:"workdir"`
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
		stdout:    s.newCappedBuffer(),
		stderr:    s.newCappedBuffer(),
		cancel:    cancel,
	}

	s.jobs.mu.Lock()
	s.jobs.pruneLocked(job.StartedAt)
	s.jobs.jobs[job.ID] = job
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
	}()
	return id, nil
}

func (j *jobStore) pruneLocked(now time.Time) {
	for id, job := range j.jobs {
		if job.FinishedAt != nil && now.Sub(*job.FinishedAt) > jobRetention {
			delete(j.jobs, id)
		}
	}
}

func (s *Server) getJob(id string) (commandJobView, bool) {
	s.jobs.mu.RLock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		s.jobs.mu.RUnlock()
		return commandJobView{}, false
	}
	view := commandJobView{
		ID:         job.ID,
		Status:     job.Status,
		Workdir:    job.Workdir,
		Error:      job.Error,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
	result := job.result
	stdout := job.stdout
	stderr := job.stderr
	s.jobs.mu.RUnlock()

	view.Stdout, view.Truncated = stdout.snapshot()
	stderrText, stderrTruncated := stderr.snapshot()
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
	return view, true
}

func (s *Server) cancelJob(id string) (string, bool) {
	s.jobs.mu.Lock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		s.jobs.mu.Unlock()
		return "", false
	}
	cancel := job.cancel
	s.jobs.mu.Unlock()
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
	job, ok := s.getJob(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelCommandJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cancelJob(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}
