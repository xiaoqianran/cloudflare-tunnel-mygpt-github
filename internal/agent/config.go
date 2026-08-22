package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address               string
	APIToken              string
	CommandTimeout        time.Duration
	MaxCommandOutputChars int
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Address:               envString("LISTEN_ADDR", "127.0.0.1:8787"),
		APIToken:              strings.TrimSpace(os.Getenv("API_TOKEN")),
		CommandTimeout:        time.Duration(envInt("COMMAND_TIMEOUT_SECONDS", 1800)) * time.Second,
		MaxCommandOutputChars: envInt("MAX_COMMAND_OUTPUT_CHARS", 180000),
	}
	if cfg.APIToken == "" {
		return Config{}, fmt.Errorf("API_TOKEN is required")
	}
	if cfg.CommandTimeout < time.Second {
		cfg.CommandTimeout = 30 * time.Minute
	}
	if cfg.MaxCommandOutputChars < 1000 {
		cfg.MaxCommandOutputChars = 180000
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
