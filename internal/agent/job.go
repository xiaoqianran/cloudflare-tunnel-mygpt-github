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
	ID         string             `json:"id"`
	Status     string             `json:"status"`
	Result     *hostCommandResult `json:"result,omitempty"`
	Error      string             `json:"error,omitempty"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
	cancel     context.CancelFunc
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
	job := &commandJob{ID: id, Status: "running", StartedAt: time.Now().UTC(), cancel: cancel}

	s.jobs.mu.Lock()
	s.jobs.pruneLocked(job.StartedAt)
	s.jobs.jobs[job.ID] = job
	s.jobs.mu.Unlock()

	go func() {
		result, err := s.runHostCommand(ctx, input.Command, input.Workdir, input.Stdin, input.TimeoutSeconds)
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
		job.Result = &result
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

func (s *Server) getJob(id string) (commandJob, bool) {
	s.jobs.mu.RLock()
	defer s.jobs.mu.RUnlock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		return commandJob{}, false
	}
	copy := *job
	copy.cancel = nil
	return copy, true
}

func (s *Server) cancelJob(id string) (commandJob, bool) {
	s.jobs.mu.Lock()
	job, ok := s.jobs.jobs[id]
	if !ok {
		s.jobs.mu.Unlock()
		return commandJob{}, false
	}
	cancel := job.cancel
	copy := *job
	copy.cancel = nil
	s.jobs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return copy, true
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
	job, ok := s.cancelJob(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": job.ID})
}
