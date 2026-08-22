package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Server struct {
	cfg    Config
	locker *Locker
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, locker: NewLocker()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.Handle("POST /v1/command/run", s.CommandEndpoint())
	return requestLogger(mux)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeError(w, http.StatusUnauthorized, "Bearer API token required")
			return
		}
		got := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
		want := []byte(s.cfg.APIToken)
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorizeRepo(raw string) (string, error) {
	repo, err := normalizeRepo(raw)
	if err != nil {
		return "", err
	}
	if !repoAllowed(repo, s.cfg.AllowedRepos) {
		return "", fmt.Errorf("repository is not allowed: %s", repo)
	}
	return repo, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("invalid JSON body: multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "mygpt-vps-root-shell",
		"version": "0.3.0",
	})
}

func (s *Server) handleSyncRepository(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo string `json:"repo"`
		Ref  string `json:"ref"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ref := strings.TrimSpace(input.Ref)
	if strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) {
		writeError(w, 400, "invalid ref")
		return
	}
	unlock := s.locker.Lock(repo)
	defer unlock()

	ctx := r.Context()
	action := "fetched"
	path := repoDir(s.cfg.WorkspaceRoot, repo)
	if !s.repoExists(repo) {
		path, err = s.cloneRepo(ctx, repo)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		action = "cloned"
	} else {
		if _, err := s.requireGitOK(ctx, path, "fetch", "--all", "--prune", "--tags"); err != nil {
			writeError(w, 502, err.Error())
			return
		}
	}

	state, err := s.repoState(ctx, path)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	dirty, _ := state["dirty"].(bool)
	worktreeUpdated := false
	if !dirty {
		if ref != "" {
			if _, err := s.requireGitOK(ctx, path, "checkout", ref); err != nil {
				writeError(w, 409, err.Error())
				return
			}
			worktreeUpdated = true
		}
		branchResult, _ := s.git(ctx, path, "branch", "--show-current")
		branch := strings.TrimSpace(branchResult.Stdout)
		if branch != "" {
			upstream, _ := s.git(ctx, path, "rev-parse", "--abbrev-ref", "@{u}")
			if upstream.ExitCode == 0 && strings.TrimSpace(upstream.Stdout) != "" {
				if _, err := s.requireGitOK(ctx, path, "merge", "--ff-only", strings.TrimSpace(upstream.Stdout)); err != nil {
					writeError(w, 409, err.Error())
					return
				}
				worktreeUpdated = true
			}
		}
	}
	state, _ = s.repoState(ctx, path)
	writeJSON(w, 200, map[string]any{
		"repo":              repo,
		"action":            action,
		"workspace":         path,
		"worktree_updated":  worktreeUpdated,
		"dirty_before_sync": dirty,
		"state":             state,
	})
}

func (s *Server) handleInspectRepository(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo   string `json:"repo"`
		Prefix string `json:"path_prefix"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	dir, err := s.ensureRepo(repo)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	paths, err := s.listRepoFiles(r.Context(), dir)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	paths = filterPaths(paths, input.Prefix)
	sort.Strings(paths)
	limit := input.Limit
	if limit < 1 || limit > 5000 {
		limit = 1000
	}
	start := 0
	if input.Cursor != "" {
		start = sort.Search(len(paths), func(i int) bool { return paths[i] > input.Cursor })
	}
	end := min(start+limit, len(paths))
	page := paths[start:end]
	var next any
	if end < len(paths) && len(page) > 0 {
		next = page[len(page)-1]
	}
	state, _ := s.repoState(r.Context(), dir)
	writeJSON(w, 200, map[string]any{
		"repo":        repo,
		"workspace":   dir,
		"state":       state,
		"file_count":  len(paths),
		"files":       page,
		"next_cursor": next,
	})
}

func (s *Server) handleReadFiles(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo  string   `json:"repo"`
		Paths []string `json:"paths"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	files, err := s.readFiles(r.Context(), repo, input.Paths)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"repo": repo, "files": files})
}

func (s *Server) handleSearchRepository(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo   string `json:"repo"`
		Query  string `json:"query"`
		Prefix string `json:"path_prefix"`
		Regex  bool   `json:"regex"`
		Limit  int    `json:"limit"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	matches, engine, err := s.searchRepo(r.Context(), repo, input.Query, input.Prefix, input.Regex, input.Limit)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"repo": repo, "engine": engine, "matches": matches})
}

func (s *Server) handleReadRepositoryPage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo     string `json:"repo"`
		Prefix   string `json:"path_prefix"`
		Cursor   string `json:"cursor"`
		MaxChars int    `json:"max_chars"`
		MaxFiles int    `json:"max_files"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	files, next, chars, err := s.readRepoPage(r.Context(), repo, input.Prefix, input.Cursor, input.MaxChars, input.MaxFiles)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"repo": repo, "files": files, "returned_chars": chars, "next_cursor": next})
}

func (s *Server) handleApplyChanges(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo    string       `json:"repo"`
		Changes []FileChange `json:"changes"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	unlock := s.locker.Lock(repo)
	defer unlock()
	results, err := s.applyFileChanges(repo, input.Changes)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	dir, _ := s.ensureRepo(repo)
	status, _ := s.git(r.Context(), dir, "status", "--short")
	writeJSON(w, 200, map[string]any{"repo": repo, "changes": results, "git_status": strings.TrimSpace(status.Stdout)})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo     string `json:"repo"`
		Staged   bool   `json:"staged"`
		MaxChars int    `json:"max_chars"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	dir, err := s.ensureRepo(repo)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	args := []string{"diff", "--no-ext-diff"}
	if input.Staged {
		args = append(args, "--cached")
	}
	result, err := s.requireGitOK(r.Context(), dir, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	maxChars := input.MaxChars
	if maxChars < 1 || maxChars > s.cfg.MaxDiffChars {
		maxChars = s.cfg.MaxDiffChars
	}
	diff := result.Stdout
	truncated := false
	if len(diff) > maxChars {
		diff = diff[:maxChars]
		truncated = true
	}
	writeJSON(w, 200, map[string]any{"repo": repo, "diff": diff, "truncated": truncated})
}

func validBranchName(branch string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") || strings.ContainsAny(branch, " ~^:?*[\\") {
		return false
	}
	return !strings.HasSuffix(branch, "/") && !strings.HasSuffix(branch, ".")
}

func (s *Server) handleCommitPush(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo    string `json:"repo"`
		Message string `json:"message"`
		Branch  string `json:"branch"`
		Force   bool   `json:"force"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	branch := strings.TrimSpace(input.Branch)
	if branch != "" && !validBranchName(branch) {
		writeError(w, 400, "invalid branch")
		return
	}

	unlock := s.locker.Lock(repo)
	defer unlock()
	dir, err := s.ensureRepo(repo)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	ctx := r.Context()

	status, err := s.requireGitOK(ctx, dir, "status", "--porcelain=v1")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	hasChanges := strings.TrimSpace(status.Stdout) != ""
	if hasChanges && strings.TrimSpace(input.Message) == "" {
		writeError(w, 400, "message is required when there are changes to commit")
		return
	}

	currentResult, _ := s.git(ctx, dir, "branch", "--show-current")
	current := strings.TrimSpace(currentResult.Stdout)
	if branch != "" && branch != current {
		if checkout, _ := s.git(ctx, dir, "switch", branch); checkout.ExitCode != 0 {
			if _, err := s.requireGitOK(ctx, dir, "switch", "-c", branch); err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
		current = branch
	}
	if current == "" {
		writeError(w, 409, "cannot commit-and-push from detached HEAD; specify a branch")
		return
	}
	if hasChanges {
		if _, err := s.requireGitOK(ctx, dir, "add", "-A"); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	check, err := s.git(ctx, dir, "diff", "--cached", "--quiet")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	committed := false
	if check.ExitCode == 1 {
		if _, err := s.requireGitOK(ctx, dir, "commit", "-m", input.Message); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		committed = true
	} else if check.ExitCode != 0 {
		writeError(w, 500, strings.TrimSpace(check.Stderr))
		return
	}
	pushArgs := []string{"push", "-u"}
	if input.Force {
		pushArgs = append(pushArgs, "--force")
	}
	pushArgs = append(pushArgs, "origin", "HEAD:refs/heads/"+current)
	push, err := s.requireGitOK(ctx, dir, pushArgs...)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	state, _ := s.repoState(ctx, dir)
	writeJSON(w, 200, map[string]any{
		"repo":      repo,
		"branch":    current,
		"committed": committed,
		"force":     input.Force,
		"push":      strings.TrimSpace(push.Stdout + "\n" + push.Stderr),
		"state":     state,
	})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	origin := scheme + "://" + r.Host
	spec := strings.ReplaceAll(openAPISpec, "__SERVER_URL__", origin)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(spec))
}

func EnsureWorkspace(cfg Config) error {
	if err := os.MkdirAll(cfg.WorkspaceRoot, 0o755); err != nil {
		return err
	}
	resolved, err := filepath.Abs(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	log.Printf("workspace root: %s", resolved)
	return nil
}

var _ = errors.New
