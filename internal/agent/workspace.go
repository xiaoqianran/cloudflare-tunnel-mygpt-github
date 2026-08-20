package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type repoCursor struct {
	Path   string `json:"p"`
	Offset int64  `json:"o"`
}

type Locker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewLocker() *Locker {
	return &Locker{locks: map[string]*sync.Mutex{}}
}

func (l *Locker) Lock(repo string) func() {
	l.mu.Lock()
	m := l.locks[repo]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[repo] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func normalizeRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if !repoRE.MatchString(repo) {
		return "", fmt.Errorf("repo must be owner/name")
	}
	return repo, nil
}

func repoAllowed(repo string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern == "*" || pattern == repo {
			return true
		}
		if strings.HasSuffix(pattern, "/*") && strings.HasPrefix(repo, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

func repoDir(root, repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return filepath.Join(root, owner, name)
}

func safeRepoPath(root, relative string, mustExist bool) (string, error) {
	relative = filepath.ToSlash(strings.TrimSpace(relative))
	if relative == "" || strings.HasPrefix(relative, "/") {
		return "", errors.New("path must be a non-empty repository-relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" {
			return "", errors.New(".git internals are not exposed")
		}
	}
	full := filepath.Join(root, clean)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}

	if mustExist {
		resolved, err := filepath.EvalSymlinks(fullAbs)
		if err != nil {
			return "", err
		}
		if resolved != rootAbs && !strings.HasPrefix(resolved, rootAbs+string(filepath.Separator)) {
			return "", errors.New("symlink escapes repository")
		}
		return resolved, nil
	}

	parent := filepath.Dir(fullAbs)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			if resolved != rootAbs && !strings.HasPrefix(resolved, rootAbs+string(filepath.Separator)) {
				return "", errors.New("parent symlink escapes repository")
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if parent == rootAbs {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	return fullAbs, nil
}

func encodeCursor(cursor repoCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(value string) (repoCursor, error) {
	if strings.TrimSpace(value) == "" {
		return repoCursor{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return repoCursor{}, errors.New("invalid cursor")
	}
	var cursor repoCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Offset < 0 {
		return repoCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func filterPaths(paths []string, prefix string) []string {
	prefix = strings.Trim(strings.TrimSpace(filepath.ToSlash(prefix)), "/")
	if prefix == "" {
		sort.Strings(paths)
		return paths
	}
	out := paths[:0]
	for _, path := range paths {
		p := filepath.ToSlash(path)
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
