package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCommandEndpointIsRegisteredAndAuthenticated(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token"})
	req := httptest.NewRequest(http.MethodPost, "/v1/command/run", strings.NewReader(`{"command":"id"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected authenticated command route, got status %d", rec.Code)
	}
}

func TestOnlyCommandActionIsExposed(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token"})
	for _, path := range []string{"/v1/repository/sync", "/v1/files/read", "/v1/git/commit-push", "/v1/github/release"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unexpected legacy route %s: status %d", path, rec.Code)
		}
	}
}

func TestDecodeJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	for _, body := range []string{
		`{"command":"true","unexpected":true}`,
		`{"command":"true"} {"command":"false"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		var dst struct {
			Command string `json:"command"`
		}
		if err := decodeJSON(rec, req, &dst); err == nil {
			t.Fatalf("expected invalid JSON body to be rejected: %s", body)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	buf := &cappedBuffer{limit: 5}
	if n, err := buf.Write([]byte("abcdefgh")); err != nil || n != 8 {
		t.Fatalf("unexpected write result: n=%d err=%v", n, err)
	}
	text, truncated := buf.snapshot()
	if text != "defgh" || !truncated {
		t.Fatalf("unexpected capped output: %q truncated=%v", text, truncated)
	}
	_, _ = buf.Write([]byte("ij"))
	if buf.String() != "fghij" {
		t.Fatalf("capped output should retain the tail: %q", buf.String())
	}
}

func TestOpenAPIExposesUniversalNonConsequentialRunCommand(t *testing.T) {
	var spec struct {
		Info struct {
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"info"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID     string `json:"operationId"`
			IsConsequential *bool  `json:"x-openai-isConsequential"`
			Description     string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != apiVersion {
		t.Fatalf("OpenAPI version %q does not match code version %q", spec.Info.Version, apiVersion)
	}
	if spec.Components.Schemas == nil {
		t.Fatal("components.schemas must be an object, even when empty")
	}
	if len(spec.Info.Description) > 300 {
		t.Fatalf("OpenAPI info description exceeds Builder limit: %d", len(spec.Info.Description))
	}
	if len(spec.Paths) != 4 {
		t.Fatalf("expected four action paths, got %d", len(spec.Paths))
	}
	methods, ok := spec.Paths["/v1/command/run"]
	if !ok {
		t.Fatal("runCommand path missing from OpenAPI")
	}
	op, ok := methods["post"]
	if !ok || op.OperationID != "runCommand" {
		t.Fatalf("unexpected runCommand operation: %#v", op)
	}
	if op.IsConsequential == nil || *op.IsConsequential {
		t.Fatal("runCommand action must be explicitly non-consequential to avoid per-call confirmation prompts")
	}
	for path, operationID := range map[string]string{
		"/v1/command/start":            "startCommand",
		"/v1/command/jobs/{id}":        "getCommandJob",
		"/v1/command/jobs/{id}/cancel": "cancelCommandJob",
	} {
		methods := spec.Paths[path]
		var got string
		for _, candidate := range methods {
			got = candidate.OperationID
		}
		if got != operationID {
			t.Fatalf("unexpected operation for %s: %q", path, got)
		}
	}
	if len(op.Description) > 300 {
		t.Fatalf("runCommand description exceeds Builder limit: %d", len(op.Description))
	}
	for _, phrase := range []string{"install", "workflow", "not a capability boundary"} {
		combined := strings.ToLower(spec.Info.Description + " " + op.Description)
		if !strings.Contains(combined, phrase) {
			t.Fatalf("OpenAPI should communicate universal capability; missing %q", phrase)
		}
	}
}

func TestHealthVersionMatchesOpenAPI(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status: %d", rec.Code)
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Version != apiVersion {
		t.Fatalf("health version %q does not match %q", health.Version, apiVersion)
	}
}

func TestHostCommandRunsInRequestedDirectoryAndAcceptsStdin(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	root := t.TempDir()
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	result, err := s.runHostCommand(context.Background(), `printf '%s|' "$PWD"; cat`, root, "from-stdin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != root+"|from-stdin" {
		t.Fatalf("unexpected host command result: %#v", result)
	}
}

func TestHostCommandTimeoutKillsWorkflow(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 5 * time.Second, MaxCommandOutputChars: 20000})
	workdir := t.TempDir()
	started := time.Now()
	result, err := s.runHostCommand(context.Background(), "sleep 10", workdir, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("expected timeout result, got %#v", result)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("timeout did not terminate workflow promptly: %s", time.Since(started))
	}
}

func TestAsyncCommandCompletes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf done", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := s.getJob(id)
		if !ok {
			t.Fatal("job disappeared")
		}
		if got.Status == "completed" {
			if got.ExitCode == nil || *got.ExitCode != 0 || got.Stdout != "done" {
				t.Fatalf("unexpected result: %#v", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestAsyncCommandCanBeCancelled(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "sleep 10", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.cancelJob(id); !ok {
		t.Fatal("job not found")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := s.getJob(id)
		if got.Status == "cancelled" {
			if got.ExitCode == nil || *got.ExitCode == 0 {
				t.Fatalf("unexpected cancelled result: %#v", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job was not cancelled")
}

func TestAsyncCommandExposesLiveOutput(t *testing.T) {
	s := NewServer(Config{CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{
		Command: "printf first; sleep 0.2; printf second >&2; sleep 0.2; printf third",
		Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	seenRunningOutput := false
	for time.Now().Before(deadline) {
		job, ok := s.getJob(id)
		if !ok {
			t.Fatal("job disappeared")
		}
		if job.Status == "running" && strings.Contains(job.Stdout, "first") {
			seenRunningOutput = true
			if job.ExitCode != nil {
				t.Fatalf("running job must not have an exit code: %#v", job)
			}
		}
		if job.Status == "completed" {
			if !seenRunningOutput {
				t.Fatal("job completed before live output was observable")
			}
			if job.Stdout != "firstthird" || !strings.Contains(job.Stderr, "second") || job.ExitCode == nil || *job.ExitCode != 0 {
				t.Fatalf("unexpected completed job: %#v", job)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestExplicitTimeoutOverridesDefault(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 50 * time.Millisecond, MaxCommandOutputChars: 20000})
	result, err := s.runHostCommand(context.Background(), "sleep 0.2; printf done", t.TempDir(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.TimedOut || result.ExitCode != 0 || result.Stdout != "done" {
		t.Fatalf("explicit timeout did not override default: %#v", result)
	}
}

func TestAsyncCommandHTTPFlow(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: time.Second, MaxCommandOutputChars: 20000})
	h := s.Handler()

	body, err := json.Marshal(commandInput{Command: "printf done", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/command/start", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status: %d body=%s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("invalid start response: %s", rec.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		req = httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+started.ID, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var job commandJobView
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("invalid job response: %s", rec.Body.String())
		}
		switch job.Status {
		case "completed":
			return
		case "failed", "cancelled", "timed_out":
			t.Fatalf("job ended as %s: %s", job.Status, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete through HTTP API")
}
