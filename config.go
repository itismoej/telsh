package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	BotToken       string
	AllowedUsers   map[int64]bool
	Shell          string
	ShellPrefix    []string // e.g. ["nsenter","-t","1","-m","--"] — nil if no nsenter
	SessionTimeout time.Duration
}

// LoadConfig reads configuration from environment variables and validates it.
func LoadConfig() (*Config, error) {
	token := os.Getenv("TELSH_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELSH_BOT_TOKEN is required")
	}

	usersStr := os.Getenv("TELSH_ALLOWED_USERS")
	if usersStr == "" {
		return nil, fmt.Errorf("TELSH_ALLOWED_USERS is required (comma-separated Telegram user IDs)")
	}

	allowedUsers := make(map[int64]bool)
	for _, s := range strings.Split(usersStr, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid user ID %q in TELSH_ALLOWED_USERS: %w", s, err)
		}
		allowedUsers[id] = true
	}

	if len(allowedUsers) == 0 {
		return nil, fmt.Errorf("TELSH_ALLOWED_USERS must contain at least one user ID")
	}

	shell := os.Getenv("TELSH_SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	timeoutMin := 30
	if s := os.Getenv("TELSH_SESSION_TIMEOUT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("TELSH_SESSION_TIMEOUT must be a positive integer (minutes): %w", err)
		}
		timeoutMin = n
	}

	// If the shell command uses nsenter, extract the prefix (everything up to
	// and including "--") so we can reuse it for file operations on the host.
	var shellPrefix []string
	parts := strings.Fields(shell)
	for i, p := range parts {
		if p == "--" {
			shellPrefix = parts[:i+1]
			break
		}
	}

	return &Config{
		BotToken:       token,
		AllowedUsers:   allowedUsers,
		Shell:          shell,
		ShellPrefix:    shellPrefix,
		SessionTimeout: time.Duration(timeoutMin) * time.Minute,
	}, nil
}
