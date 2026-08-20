package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type ReadFileResult struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Content string `json:"content,omitempty"`
	Binary  bool   `json:"binary"`
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

type PageFile struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	ByteStart int64  `json:"byte_start"`
	ByteEnd   int64  `json:"byte_end"`
	Partial   bool   `json:"partial"`
}

type FileChange struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

func readTextFile(path string) (ReadFileResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ReadFileResult{}, err
	}
	if !info.Mode().IsRegular() {
		return ReadFileResult{}, errors.New("path is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ReadFileResult{}, err
	}
	binary := bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data)
	result := ReadFileResult{Size: info.Size(), Binary: binary}
	if !binary {
		result.Content = string(data)
	}
	return result, nil
}

func (s *Server) readFiles(ctx context.Context, repo string, paths []string) ([]ReadFileResult, error) {
	if len(paths) == 0 {
		return nil, errors.New("paths must not be empty")
	}
	if len(paths) > s.cfg.MaxReadFiles {
		return nil, fmt.Errorf("at most %d files may be read per request", s.cfg.MaxReadFiles)
	}
	dir, err := s.ensureRepo(repo)
	if err != nil {
		return nil, err
	}
	out := make([]ReadFileResult, 0, len(paths))
	for _, path := range paths {
		full, err := safeRepoPath(dir, path, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		result, err := readTextFile(full)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		result.Path = filepath.ToSlash(path)
		out = append(out, result)
	}
	return out, nil
}

func (s *Server) searchRepo(ctx context.Context, repo, query, prefix string, regexMode bool, limit int) ([]SearchMatch, string, error) {
	if strings.TrimSpace(query) == "" {
		return nil, "", errors.New("query is required")
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	dir, err := s.ensureRepo(repo)
	if err != nil {
		return nil, "", err
	}
	if _, err := exec.LookPath("rg"); err == nil {
		matches, err := s.searchWithRipgrep(ctx, dir, query, prefix, regexMode, limit)
		return matches, "ripgrep", err
	}
	matches, err := s.searchFallback(ctx, dir, query, prefix, regexMode, limit)
	return matches, "go-fallback", err
}

func (s *Server) searchWithRipgrep(ctx context.Context, dir, query, prefix string, regexMode bool, limit int) ([]SearchMatch, error) {
	target := "."
	if strings.TrimSpace(prefix) != "" {
		full, err := safeRepoPath(dir, prefix, true)
		if err != nil {
			return nil, err
		}
		target, err = filepath.Rel(dir, full)
		if err != nil {
			return nil, err
		}
	}
	args := []string{"--json", "--hidden", "--glob", "!.git/**", "--no-messages"}
	if !regexMode {
		args = append(args, "-F")
	}
	args = append(args, "--", query, target)
	result, err := s.runCommand(ctx, dir, "rg", args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 && result.ExitCode != 1 {
		return nil, errors.New(strings.TrimSpace(result.Stderr))
	}

	type rgEvent struct {
		Type string `json:"type"`
		Data struct {
			Path struct{ Text string `json:"text"` } `json:"path"`
			Lines struct{ Text string `json:"text"` } `json:"lines"`
			LineNumber int `json:"line_number"`
			Submatches []struct {
				Start int `json:"start"`
			} `json:"submatches"`
		} `json:"data"`
	}

	matches := make([]SearchMatch, 0, min(limit, 100))
	scanner := bufio.NewScanner(strings.NewReader(result.Stdout))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 2*1024*1024)
	for scanner.Scan() {
		var event rgEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "match" {
			continue
		}
		column := 1
		if len(event.Data.Submatches) > 0 {
			column = event.Data.Submatches[0].Start + 1
		}
		matches = append(matches, SearchMatch{
			Path:   filepath.ToSlash(strings.TrimPrefix(event.Data.Path.Text, "./")),
			Line:   event.Data.LineNumber,
			Column: column,
			Text:   strings.TrimRight(event.Data.Lines.Text, "\r\n"),
		})
		if len(matches) >= limit {
			break
		}
	}
	return matches, scanner.Err()
}

func (s *Server) searchFallback(ctx context.Context, dir, query, prefix string, regexMode bool, limit int) ([]SearchMatch, error) {
	paths, err := s.listRepoFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	paths = filterPaths(paths, prefix)
	var re *regexp.Regexp
	if regexMode {
		re, err = regexp.Compile(query)
		if err != nil {
			return nil, err
		}
	}
	matches := make([]SearchMatch, 0, min(limit, 100))
	for _, path := range paths {
		full, err := safeRepoPath(dir, path, true)
		if err != nil {
			continue
		}
		file, err := os.Open(full)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			index := -1
			if re != nil {
				loc := re.FindStringIndex(line)
				if loc != nil {
					index = loc[0]
				}
			} else {
				index = strings.Index(line, query)
			}
			if index >= 0 {
				matches = append(matches, SearchMatch{Path: path, Line: lineNo, Column: index + 1, Text: line})
				if len(matches) >= limit {
					_ = file.Close()
					return matches, nil
				}
			}
		}
		_ = file.Close()
	}
	return matches, nil
}

func (s *Server) readRepoPage(ctx context.Context, repo, prefix, cursorValue string, maxChars, maxFiles int) ([]PageFile, *string, int, error) {
	dir, err := s.ensureRepo(repo)
	if err != nil {
		return nil, nil, 0, err
	}
	if maxChars < 10_000 || maxChars > s.cfg.MaxPageChars {
		maxChars = s.cfg.MaxPageChars
	}
	if maxFiles < 1 || maxFiles > 100 {
		maxFiles = 40
	}
	cursor, err := decodeCursor(cursorValue)
	if err != nil {
		return nil, nil, 0, err
	}
	paths, err := s.listRepoFiles(ctx, dir)
	if err != nil {
		return nil, nil, 0, err
	}
	paths = filterPaths(paths, prefix)
	sort.Strings(paths)

	start := 0
	if cursor.Path != "" {
		start = sort.SearchStrings(paths, cursor.Path)
	}
	files := make([]PageFile, 0, min(maxFiles, 40))
	used := 0

	for i := start; i < len(paths) && len(files) < maxFiles; i++ {
		path := paths[i]
		offset := int64(0)
		if path == cursor.Path {
			offset = cursor.Offset
		}
		full, err := safeRepoPath(dir, path, true)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil || len(data) == 0 {
			continue
		}
		if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data) {
			continue
		}
		if offset >= int64(len(data)) {
			continue
		}
		remaining := maxChars - used
		if remaining <= 0 {
			next := encodeCursor(repoCursor{Path: path, Offset: offset})
			return files, &next, used, nil
		}
		end := len(data)
		if int64(end)-offset > int64(remaining) {
			end = int(offset) + remaining
			for end > int(offset) && !utf8.Valid(data[offset:end]) {
				end--
			}
			if end <= int(offset) {
				return nil, nil, 0, errors.New("unable to split UTF-8 file within page budget")
			}
			chunk := string(data[offset:end])
			files = append(files, PageFile{Path: path, Content: chunk, ByteStart: offset, ByteEnd: int64(end), Partial: true})
			used += len(chunk)
			next := encodeCursor(repoCursor{Path: path, Offset: int64(end)})
			return files, &next, used, nil
		}
		chunk := string(data[offset:])
		files = append(files, PageFile{Path: path, Content: chunk, ByteStart: offset, ByteEnd: int64(len(data)), Partial: offset > 0})
		used += len(chunk)
	}

	if len(files) == maxFiles {
		last := files[len(files)-1]
		idx := sort.SearchStrings(paths, last.Path)
		if idx+1 < len(paths) {
			next := encodeCursor(repoCursor{Path: paths[idx+1], Offset: 0})
			return files, &next, used, nil
		}
	}
	return files, nil, used, nil
}

func (s *Server) applyFileChanges(repo string, changes []FileChange) ([]map[string]any, error) {
	if len(changes) == 0 {
		return nil, errors.New("changes must not be empty")
	}
	dir, err := s.ensureRepo(repo)
	if err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(changes))
	seen := map[string]struct{}{}
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if _, exists := seen[path]; exists {
			return nil, fmt.Errorf("duplicate path: %s", path)
		}
		seen[path] = struct{}{}
		full, err := safeRepoPath(dir, path, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if change.Delete {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			results = append(results, map[string]any{"path": path, "action": "deleted"})
			continue
		}
		if int64(len(change.Content)) > s.cfg.MaxWriteBytes {
			return nil, fmt.Errorf("%s exceeds MAX_WRITE_BYTES (%d)", path, s.cfg.MaxWriteBytes)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		temp, err := os.CreateTemp(filepath.Dir(full), ".mygpt-write-*")
		if err != nil {
			return nil, err
		}
		tempName := temp.Name()
		if _, err := io.WriteString(temp, change.Content); err != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
			return nil, err
		}
		if err := temp.Chmod(0o644); err != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
			return nil, err
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempName)
			return nil, err
		}
		if err := os.Rename(tempName, full); err != nil {
			_ = os.Remove(tempName)
			return nil, err
		}
		results = append(results, map[string]any{"path": path, "action": "written", "bytes": len(change.Content)})
	}
	return results, nil
}
