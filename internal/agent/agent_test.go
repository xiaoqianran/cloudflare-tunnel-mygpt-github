package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
		WorkspaceRoot:  root,
		APIToken:       "test-token",
		CommandTimeout: 10 * time.Second,
		MaxReadFiles:   50,
		MaxPageChars:   20_000,
		MaxWriteBytes:  1_000_000,
		MaxDiffChars:   20_000,
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
