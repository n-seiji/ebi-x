// Package config loads ebiii's runtime configuration.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultCodexTimeout               = 30 * time.Minute
	defaultThreadSubscriptionReaction = "thread-subete"
	defaultThreadSubscriptionTTL      = 336 * time.Hour
)

// Config contains ebiii's runtime configuration and resolved data paths.
type Config struct {
	SlackBotToken              string
	SlackAppToken              string
	AllowedUserIDs             []string
	AllowedChannelIDs          []string
	AllowWorkflows             bool
	AdminUserID                string
	CodexCommand               string
	CodexModel                 string
	CodexTimeout               time.Duration
	ThreadSubscriptionReaction string
	ThreadSubscriptionTTL      time.Duration
	EBIIIHome                  string
	WorkspaceDir               string
	MemoryDir                  string
	PlaybooksDir               string
	StateDir                   string
	// WritableRoots are absolute, symlink-resolved directories the work turn
	// may write to in addition to the workspace.
	WritableRoots []string
}

// Load reads configuration from the environment and a local .env file.
//
// Values already present in the process environment take precedence over
// values in .env.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}

	botToken, err := requiredEnv("SLACK_BOT_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN: %w", err)
	}
	appToken, err := requiredEnv("SLACK_APP_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("SLACK_APP_TOKEN: %w", err)
	}

	userIDs := splitList(os.Getenv("SLACK_ALLOWED_USER_IDS"))
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("SLACK_ALLOWED_USER_IDS: %w", errors.New("must contain at least one user ID"))
	}
	for _, userID := range userIDs {
		if !strings.HasPrefix(userID, "U") {
			return nil, fmt.Errorf("SLACK_ALLOWED_USER_IDS: invalid user ID %q: %w", userID, errors.New("must start with U"))
		}
	}
	allowWorkflows := false
	if value := strings.TrimSpace(os.Getenv("SLACK_ALLOW_WORKFLOWS")); value != "" {
		allowWorkflows, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("SLACK_ALLOW_WORKFLOWS %q: %w", value, err)
		}
	}
	adminUserID := strings.TrimSpace(os.Getenv("SLACK_ADMIN_USER_ID"))
	if adminUserID != "" && !strings.HasPrefix(adminUserID, "U") {
		return nil, fmt.Errorf("SLACK_ADMIN_USER_ID: invalid user ID %q: %w", adminUserID, errors.New("must start with U"))
	}

	codexCommand := os.Getenv("CODEX_COMMAND")
	if codexCommand == "" {
		codexCommand = "codex"
	} else if strings.IndexFunc(codexCommand, unicode.IsSpace) >= 0 {
		return nil, fmt.Errorf("CODEX_COMMAND %q: %w", codexCommand, errors.New("must be an executable path without whitespace"))
	}

	codexTimeout := defaultCodexTimeout
	if value := strings.TrimSpace(os.Getenv("CODEX_TIMEOUT")); value != "" {
		codexTimeout, err = time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("CODEX_TIMEOUT %q: %w", value, err)
		}
	}
	threadSubscriptionReaction := defaultThreadSubscriptionReaction
	if value, exists := os.LookupEnv("SLACK_THREAD_SUBSCRIPTION_REACTION"); exists {
		threadSubscriptionReaction = strings.TrimSpace(value)
	}
	threadSubscriptionTTL := defaultThreadSubscriptionTTL
	if value := strings.TrimSpace(os.Getenv("SLACK_THREAD_SUBSCRIPTION_TTL")); value != "" {
		threadSubscriptionTTL, err = time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("SLACK_THREAD_SUBSCRIPTION_TTL %q: %w", value, err)
		}
	}
	if threadSubscriptionReaction != "" && threadSubscriptionTTL <= 0 {
		return nil, fmt.Errorf("SLACK_THREAD_SUBSCRIPTION_TTL %q: %w", os.Getenv("SLACK_THREAD_SUBSCRIPTION_TTL"), errors.New("must be positive when subscriptions are enabled"))
	}

	home := strings.TrimSpace(os.Getenv("EBIII_HOME"))
	if home == "" {
		home = "."
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("EBIII_HOME %q: %w", home, err)
	}
	home = absoluteHome

	writableRoots, err := resolveWritableRoots(os.Getenv("EBIII_WRITABLE_ROOTS"))
	if err != nil {
		return nil, fmt.Errorf("EBIII_WRITABLE_ROOTS: %w", err)
	}
	workspaceDir := filepath.Join(home, "data", "workspace")
	memoryDir := filepath.Join(home, "data", "memory")
	playbooksDir, err := canonicalPath(filepath.Join(home, "data", "playbooks"))
	if err != nil {
		return nil, fmt.Errorf("resolve playbooks directory: %w", err)
	}
	isolationRoots := append(append([]string(nil), writableRoots...), playbooksDir)
	if err := validateMemoryIsolation(workspaceDir, memoryDir, isolationRoots); err != nil {
		return nil, err
	}

	return &Config{
		SlackBotToken:              botToken,
		SlackAppToken:              appToken,
		AllowedUserIDs:             userIDs,
		AllowedChannelIDs:          splitList(os.Getenv("SLACK_ALLOWED_CHANNEL_IDS")),
		AllowWorkflows:             allowWorkflows,
		AdminUserID:                adminUserID,
		CodexCommand:               codexCommand,
		CodexModel:                 strings.TrimSpace(os.Getenv("CODEX_MODEL")),
		CodexTimeout:               codexTimeout,
		ThreadSubscriptionReaction: threadSubscriptionReaction,
		ThreadSubscriptionTTL:      threadSubscriptionTTL,
		EBIIIHome:                  home,
		WorkspaceDir:               workspaceDir,
		MemoryDir:                  memoryDir,
		PlaybooksDir:               playbooksDir,
		StateDir:                   filepath.Join(home, "data", "state"),
		WritableRoots:              writableRoots,
	}, nil
}

// validateMemoryIsolation keeps the agent's workspace and every additional
// sandbox root disjoint from memory. Codex receives only the current scoped
// memory through its prompt; it must never be able to inspect the backing
// files directly.
func validateMemoryIsolation(workspaceDir, memoryDir string, writableRoots []string) error {
	workspace, err := canonicalPath(workspaceDir)
	if err != nil {
		return fmt.Errorf("resolve workspace directory: %w", err)
	}
	memory, err := canonicalPath(memoryDir)
	if err != nil {
		return fmt.Errorf("resolve memory directory: %w", err)
	}
	if pathsOverlap(workspace, memory) {
		return errors.New("workspace overlaps protected memory directory")
	}
	for _, root := range writableRoots {
		if pathsOverlap(root, memory) {
			return fmt.Errorf("EBIII_WRITABLE_ROOTS: %q overlaps protected memory directory", root)
		}
	}
	return nil
}

// canonicalPath resolves symlinks in the existing prefix while still
// supporting paths whose final components have not been created yet.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, m := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, m)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathsOverlap(a, b string) bool {
	return pathContains(a, b) || pathContains(b, a)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// resolveWritableRoots parses a comma-separated list of directories into
// absolute, symlink-resolved paths with duplicates removed.
//
// Every entry must already exist and be a directory. A writable root widens
// the agent's sandbox, so a typo is rejected at startup rather than silently
// granting nothing.
func resolveWritableRoots(value string) ([]string, error) {
	var roots []string
	seen := make(map[string]struct{})
	for _, item := range splitList(value) {
		expanded, err := expandHome(item)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		absolute, err := filepath.Abs(expanded)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%q: %w", item, errors.New("must be a directory"))
		}
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		roots = append(roots, resolved)
	}
	return roots, nil
}

// expandHome replaces a leading ~ or ~/ with the current user's home
// directory. The ~user form is not supported.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		if strings.HasPrefix(path, "~") {
			return "", errors.New("~user expansion is not supported")
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", errors.New("is required")
	}
	return value, nil
}

func splitList(value string) []string {
	var result []string
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return fmt.Errorf("parse %s line %d: %w", path, lineNumber, errors.New("expected KEY=VALUE"))
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s from %s: %w", key, path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}
