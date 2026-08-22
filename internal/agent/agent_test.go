package agent

import (
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
	if buf.String() != "abcde" || !buf.truncated {
		t.Fatalf("unexpected capped output: %q truncated=%v", buf.String(), buf.truncated)
	}
}

func TestOpenAPIExposesUniversalConsequentialRunCommand(t *testing.T) {
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
	if len(spec.Paths) != 1 {
		t.Fatalf("expected exactly one action path, got %d", len(spec.Paths))
	}
	methods, ok := spec.Paths["/v1/command/run"]
	if !ok {
		t.Fatal("runCommand path missing from OpenAPI")
	}
	op, ok := methods["post"]
	if !ok || op.OperationID != "runCommand" {
		t.Fatalf("unexpected runCommand operation: %#v", op)
	}
	if op.IsConsequential == nil || !*op.IsConsequential {
		t.Fatal("root shell action must be explicitly consequential")
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
	started := time.Now()
	result, err := s.runHostCommand(context.Background(), "sleep 10", "", "", 1)
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
