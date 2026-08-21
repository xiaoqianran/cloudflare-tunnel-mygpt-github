package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepoAllowed(t *testing.T) {
	if !repoAllowed("xiaoqianran/demo", nil) {
		t.Fatal("empty allowlist should allow all repositories")
	}
	if !repoAllowed("xiaoqianran/demo", []string{"xiaoqianran/*"}) {
		t.Fatal("owner wildcard should match")
	}
	if repoAllowed("other/demo", []string{"xiaoqianran/*"}) {
		t.Fatal("different owner should not match")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := repoCursor{Path: "src/main.go", Offset: 12345}
	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cursor mismatch: got %#v want %#v", got, want)
	}
}

func TestSafeRepoPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := safeRepoPath(root, "a.txt", true); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	for _, bad := range []string{"../secret", "/etc/passwd", ".git/config"} {
		if _, err := safeRepoPath(root, bad, false); err == nil {
			t.Fatalf("unsafe path accepted: %s", bad)
		}
	}
}

func TestCommandEndpointIsRegisteredAndAuthenticated(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token"})
	req := httptest.NewRequest(http.MethodPost, "/v1/command/run", strings.NewReader(`{"command":"id"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected authenticated command route, got status %d", rec.Code)
	}
}

func TestDecodeJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	tests := []string{
		`{"repo":"alice/demo","unexpected":true}`,
		`{"repo":"alice/demo"} {"repo":"bob/demo"}`,
	}
	for _, body := range tests {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		var dst struct {
			Repo string `json:"repo"`
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

func TestHealthVersionMatchesOpenAPI(t *testing.T) {
	var spec struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != "0.2.3" {
		t.Fatalf("unexpected OpenAPI version: %s", spec.Info.Version)
	}

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
	if health.Version != spec.Info.Version {
		t.Fatalf("health version %q does not match OpenAPI version %q", health.Version, spec.Info.Version)
	}
}

func TestHostCommandRunsOnHost(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	root := t.TempDir()
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20_000})
	result, err := s.runHostCommand(context.Background(), "printf host && printf %s \"$PWD\"", root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "host"+root {
		t.Fatalf("unexpected host command result: %#v", result)
	}
}

func TestLocalWorkspaceReadWriteAndPage(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repo := "alice/demo"
	dir := repoDir(root, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello local workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "a.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}

	s := NewServer(Config{
		WorkspaceRoot:         root,
		APIToken:              "test-token",
		CommandTimeout:        10 * time.Second,
		MaxCommandOutputChars: 20_000,
		MaxReadFiles:          50,
		MaxPageChars:          20_000,
		MaxWriteBytes:         1_000_000,
		MaxDiffChars:          20_000,
	})
	files, err := s.readFiles(context.Background(), repo, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Content != "hello local workspace\n" {
		t.Fatalf("unexpected read result: %#v", files)
	}

	if _, err := s.applyFileChanges(repo, []FileChange{{Path: "src/b.txt", Content: "second file\n"}}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "src", "b.txt")); err != nil || string(data) != "second file\n" {
		t.Fatalf("write failed: %v %q", err, data)
	}

	page, next, _, err := s.readRepoPage(context.Background(), repo, "", "", 20_000, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("expected two text files, got %d", len(page))
	}
	if next != nil {
		t.Fatalf("unexpected next cursor: %v", *next)
	}
}

func TestCommitPushMissingMessageDoesNotStageChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repo := "alice/demo"
	dir := repoDir(root, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-qm", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewServer(Config{WorkspaceRoot: root, APIToken: "test-token", CommandTimeout: 10 * time.Second})
	req := httptest.NewRequest(http.MethodPost, "/v1/git/commit-push", strings.NewReader(`{"repo":"alice/demo"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}

	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed request staged changes unexpectedly: %v", err)
	}
}
