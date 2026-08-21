package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateReleaseUsesHostGHWithoutShell(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	notesFile := filepath.Join(t.TempDir(), "notes.txt")
	fakeGH := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_GH_ARGS"
cat > "$FAKE_GH_NOTES"
printf 'https://github.com/alice/demo/releases/tag/v1.2.3\n'
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_ARGS", argsFile)
	t.Setenv("FAKE_GH_NOTES", notesFile)

	s := NewServer(Config{
		APIToken:       "test-token",
		AllowedRepos:   []string{"alice/demo"},
		CommandTimeout: 10 * time.Second,
	})
	body := `{"repo":"alice/demo","tag":"v1.2.3","title":"Release 1.2.3","notes":"hello\nworld","target":"main","draft":true,"prerelease":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/github/release", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "release\ncreate\nv1.2.3\n--repo\nalice/demo\n--notes-file\n-\n--title\nRelease 1.2.3\n--target\nmain\n--draft\n--prerelease\n"
	if string(args) != wantArgs {
		t.Fatalf("unexpected gh args:\n%s", args)
	}
	notes, err := os.ReadFile(notesFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(notes) != "hello\nworld" {
		t.Fatalf("unexpected notes: %q", notes)
	}
}

func TestCreateReleaseRejectsUnauthorizedRepo(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", AllowedRepos: []string{"alice/allowed"}, CommandTimeout: time.Second})
	req := httptest.NewRequest(http.MethodPost, "/v1/github/release", strings.NewReader(`{"repo":"alice/other","tag":"v1.0.0"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAPIOperationsAreNonConsequentialAndIncludesCreateRelease(t *testing.T) {
	var spec struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			OperationID     string `json:"operationId"`
			IsConsequential *bool  `json:"x-openai-isConsequential"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != "0.2.2" {
		t.Fatalf("unexpected OpenAPI version: %s", spec.Info.Version)
	}
	foundRelease := false
	for path, methods := range spec.Paths {
		for method, op := range methods {
			if op.OperationID == "" {
				continue
			}
			if op.IsConsequential == nil || *op.IsConsequential {
				t.Fatalf("%s %s (%s) is not explicitly non-consequential", method, path, op.OperationID)
			}
			if op.OperationID == "createRelease" {
				foundRelease = true
			}
		}
	}
	if !foundRelease {
		t.Fatal("createRelease operation missing from OpenAPI")
	}
}
