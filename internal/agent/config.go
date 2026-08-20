package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address            string
	WorkspaceRoot      string
	APIToken           string
	AllowedRepos       []string
	GitRemoteBaseURL   string
	GitRemoteUsername  string
	GitRemoteToken     string
	CommandTimeout     time.Duration
	MaxReadFiles       int
	MaxPageChars       int
	MaxWriteBytes      int64
	MaxDiffChars       int
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Address:           envString("LISTEN_ADDR", "127.0.0.1:8787"),
		WorkspaceRoot:     envString("WORKSPACE_ROOT", "/srv/mygpt/repos"),
		APIToken:          strings.TrimSpace(os.Getenv("API_TOKEN")),
		AllowedRepos:      splitCSV(os.Getenv("ALLOWED_REPOS")),
		GitRemoteBaseURL:  strings.TrimRight(envString("GIT_REMOTE_BASE_URL", "https://github.com"), "/"),
		GitRemoteUsername: envString("GIT_REMOTE_USERNAME", "x-access-token"),
		GitRemoteToken:    strings.TrimSpace(os.Getenv("GIT_REMOTE_TOKEN")),
		CommandTimeout:    time.Duration(envInt("COMMAND_TIMEOUT_SECONDS", 300)) * time.Second,
		MaxReadFiles:      envInt("MAX_READ_FILES", 50),
		MaxPageChars:      envInt("MAX_PAGE_CHARS", 180000),
		MaxWriteBytes:     int64(envInt("MAX_WRITE_BYTES", 5_000_000)),
		MaxDiffChars:      envInt("MAX_DIFF_CHARS", 180000),
	}
	if cfg.GitRemoteToken == "" {
		cfg.GitRemoteToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if cfg.APIToken == "" {
		return Config{}, fmt.Errorf("API_TOKEN is required")
	}
	if cfg.MaxReadFiles < 1 {
		cfg.MaxReadFiles = 50
	}
	if cfg.MaxPageChars < 10_000 {
		cfg.MaxPageChars = 180_000
	}
	if cfg.MaxWriteBytes < 1 {
		cfg.MaxWriteBytes = 5_000_000
	}
	return cfg, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}
