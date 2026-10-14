// Package config loads ebi-x's runtime configuration.
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
	defaultMaxParallelWork            = 3
	defaultCheckoutIdleTTL            = 120 * time.Hour
)

// Config contains ebi-x's runtime configuration and resolved data paths.
type Config struct {
	SlackBotToken     string
	SlackAppToken     string
	AllowedUserIDs    []string
	AllowedChannelIDs []string
	// ApprovalChannelID is the channel where users, channels, and workflows
	// outside the allowlists are approved. Empty disables approvals.
	ApprovalChannelID string
	// ApproverUserIDs are the only users who may decide approvals.
	ApproverUserIDs []string
	AllowWorkflows  bool
	// AllowedWorkflowIDs are the Slack workflows that may mention ebi-x when
	// AllowWorkflows is set.
	AllowedWorkflowIDs []string
	AdminUserID        string
	CodexCommand       string
	CodexModel         string
	CodexTimeout       time.Duration
	// MaxParallelWork is how many work turns for different Slack threads may
	// run at once.
	MaxParallelWork int
	// SharedWriteChannelIDs are the channels whose work turns may change state
	// shared by every channel: playbooks and global memory.
	SharedWriteChannelIDs []string
	// CheckoutIdleTTL is how long a thread's git checkouts are kept after its
	// last work turn.
	CheckoutIdleTTL            time.Duration
	ThreadSubscriptionReaction string
	ThreadSubscriptionTTL      time.Duration
	EBIXHome                   string
	WorkspaceDir               string
	CheckoutsDir               string
	MemoryDir                  string
	PlaybooksDir               string
	StateDir                   string
	ActionRulesFile            string
	// WritableRoots are absolute, symlink-resolved directories the work turn
	// may write to in addition to the workspace.
	WritableRoots []string
	// ProtectedPaths are files and directories that model-generated commands
	// must neither read nor write: memory, bot state, secrets, and Codex
	// credentials and session history.
	ProtectedPaths []string
	// CodexHome is the canonical Codex home. Its credentials, state, and
	// session history are hidden from model-generated commands.
	CodexHome string
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

	approvalChannelID := strings.TrimSpace(os.Getenv("SLACK_APPROVAL_CHANNEL_ID"))
	if approvalChannelID != "" && !strings.HasPrefix(approvalChannelID, "C") && !strings.HasPrefix(approvalChannelID, "G") {
		return nil, fmt.Errorf("SLACK_APPROVAL_CHANNEL_ID: invalid channel ID %q: %w", approvalChannelID, errors.New("must start with C or G"))
	}
	approvals := approvalChannelID != ""
	approverIDs := splitList(os.Getenv("SLACK_APPROVER_USER_IDS"))
	for _, userID := range approverIDs {
		if !strings.HasPrefix(userID, "U") {
			return nil, fmt.Errorf("SLACK_APPROVER_USER_IDS: invalid user ID %q: %w", userID, errors.New("must start with U"))
		}
	}
	if approvals && len(approverIDs) == 0 {
		return nil, fmt.Errorf("SLACK_APPROVER_USER_IDS: %w", errors.New("must contain at least one user ID when SLACK_APPROVAL_CHANNEL_ID is set"))
	}

	userIDs := splitList(os.Getenv("SLACK_ALLOWED_USER_IDS"))
	if len(userIDs) == 0 && !approvals {
		return nil, fmt.Errorf("SLACK_ALLOWED_USER_IDS: %w", errors.New("must contain at least one user ID unless SLACK_APPROVAL_CHANNEL_ID is set"))
	}
	for _, userID := range userIDs {
		if !strings.HasPrefix(userID, "U") {
			return nil, fmt.Errorf("SLACK_ALLOWED_USER_IDS: invalid user ID %q: %w", userID, errors.New("must start with U"))
		}
	}
	channelIDs := splitList(os.Getenv("SLACK_ALLOWED_CHANNEL_IDS"))
	// Every public channel used to be allowable at once. Channels are now
	// listed or approved one by one, so refuse to start rather than silently
	// narrow an operator's setup.
	if value := strings.TrimSpace(os.Getenv("SLACK_ALLOW_ALL_PUBLIC_CHANNELS")); value != "" {
		allowAll, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("SLACK_ALLOW_ALL_PUBLIC_CHANNELS %q: %w", value, err)
		}
		if allowAll {
			return nil, fmt.Errorf("SLACK_ALLOW_ALL_PUBLIC_CHANNELS: %w", errors.New("was removed; list channels in SLACK_ALLOWED_CHANNEL_IDS or approve them through SLACK_APPROVAL_CHANNEL_ID"))
		}
	}
	// An empty list used to allow every conversation, including DMs and
	// private channels. Require an explicit choice instead.
	if len(channelIDs) == 0 && !approvals {
		return nil, fmt.Errorf("SLACK_ALLOWED_CHANNEL_IDS: %w", errors.New("must contain at least one channel ID unless SLACK_APPROVAL_CHANNEL_ID is set"))
	}
	allowWorkflows := false
	if value := strings.TrimSpace(os.Getenv("SLACK_ALLOW_WORKFLOWS")); value != "" {
		allowWorkflows, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("SLACK_ALLOW_WORKFLOWS %q: %w", value, err)
		}
	}
	workflowIDs := splitList(os.Getenv("SLACK_ALLOWED_WORKFLOW_IDS"))
	for _, workflowID := range workflowIDs {
		if !validWorkflowID(workflowID) {
			return nil, fmt.Errorf("SLACK_ALLOWED_WORKFLOW_IDS: invalid workflow ID %q: %w", workflowID, errors.New("must start with Wf followed by uppercase letters or digits"))
		}
	}
	// Anyone who can build a workflow could otherwise bypass the user
	// allowlist, so workflows are listed or approved one by one.
	if allowWorkflows && len(workflowIDs) == 0 && !approvals {
		return nil, fmt.Errorf("SLACK_ALLOWED_WORKFLOW_IDS: %w", errors.New("must contain at least one workflow ID when SLACK_ALLOW_WORKFLOWS=true unless SLACK_APPROVAL_CHANNEL_ID is set"))
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
	maxParallelWork, err := positiveIntEnv("CODEX_MAX_PARALLEL_WORK", defaultMaxParallelWork)
	if err != nil {
		return nil, err
	}
	sharedWriteChannelIDs := splitList(os.Getenv("EBIX_SHARED_WRITE_CHANNEL_IDS"))
	for _, channelID := range sharedWriteChannelIDs {
		if !strings.HasPrefix(channelID, "C") {
			return nil, fmt.Errorf("EBIX_SHARED_WRITE_CHANNEL_IDS: invalid channel ID %q: %w", channelID, errors.New("must start with C"))
		}
	}
	checkoutIdleTTL := defaultCheckoutIdleTTL
	if value := strings.TrimSpace(os.Getenv("EBIX_CHECKOUT_IDLE_TTL")); value != "" {
		checkoutIdleTTL, err = time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("EBIX_CHECKOUT_IDLE_TTL %q: %w", value, err)
		}
		if checkoutIdleTTL <= 0 {
			return nil, fmt.Errorf("EBIX_CHECKOUT_IDLE_TTL %q: %w", value, errors.New("must be positive"))
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

	home := strings.TrimSpace(os.Getenv("EBIX_HOME"))
	if home == "" {
		home = "."
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("EBIX_HOME %q: %w", home, err)
	}
	home = absoluteHome

	writableRoots, err := resolveWritableRoots(os.Getenv("EBIX_WRITABLE_ROOTS"))
	if err != nil {
		return nil, fmt.Errorf("EBIX_WRITABLE_ROOTS: %w", err)
	}
	workspaceDir := filepath.Join(home, "data", "workspace")
	checkoutsDir := filepath.Join(home, "data", "checkouts")
	memoryDir := filepath.Join(home, "data", "memory")
	playbooksDir, err := canonicalPath(filepath.Join(home, "data", "playbooks"))
	if err != nil {
		return nil, fmt.Errorf("resolve playbooks directory: %w", err)
	}
	stateDir := filepath.Join(home, "data", "state")
	// The operator's action rules; the agent may read them but not change them.
	actionRulesFile := filepath.Join(home, "data", "rules.md")
	codexHome, err := resolveCodexHome()
	if err != nil {
		return nil, err
	}
	protectedPaths, err := resolveProtectedPaths(home, memoryDir, stateDir, os.Getenv("EBIX_DENIED_READ_PATHS"))
	if err != nil {
		return nil, err
	}
	// Nothing the agent can write may reach a protected path or the Codex
	// home. The bot-managed directories are reserved as well: the bot grants
	// playbooks only to shared-write channels and each thread only its own
	// workspace and checkouts, which an operator-supplied root would bypass.
	botDirs := []string{workspaceDir, playbooksDir, checkoutsDir}
	reserved := append(append([]string(nil), protectedPaths...), codexHome)
	if err := validateIsolation(botDirs, reserved); err != nil {
		return nil, err
	}
	if err := validateIsolation(writableRoots, append(append(reserved, botDirs...), actionRulesFile)); err != nil {
		return nil, err
	}

	return &Config{
		SlackBotToken:              botToken,
		SlackAppToken:              appToken,
		AllowedUserIDs:             userIDs,
		AllowedChannelIDs:          channelIDs,
		ApprovalChannelID:          approvalChannelID,
		ApproverUserIDs:            approverIDs,
		AllowWorkflows:             allowWorkflows,
		AllowedWorkflowIDs:         workflowIDs,
		AdminUserID:                adminUserID,
		CodexCommand:               codexCommand,
		CodexModel:                 strings.TrimSpace(os.Getenv("CODEX_MODEL")),
		CodexTimeout:               codexTimeout,
		MaxParallelWork:            maxParallelWork,
		SharedWriteChannelIDs:      sharedWriteChannelIDs,
		CheckoutIdleTTL:            checkoutIdleTTL,
		ThreadSubscriptionReaction: threadSubscriptionReaction,
		ThreadSubscriptionTTL:      threadSubscriptionTTL,
		EBIXHome:                   home,
		WorkspaceDir:               workspaceDir,
		CheckoutsDir:               checkoutsDir,
		MemoryDir:                  memoryDir,
		PlaybooksDir:               playbooksDir,
		StateDir:                   stateDir,
		ActionRulesFile:            actionRulesFile,
		WritableRoots:              writableRoots,
		ProtectedPaths:             protectedPaths,
		CodexHome:                  codexHome,
	}, nil
}

// resolveCodexHome returns the canonical Codex home: CODEX_HOME, or
// ~/.codex when it is unset.
func resolveCodexHome() (string, error) {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = "~/.codex"
	}
	codexHome, err := expandHome(codexHome)
	if err != nil {
		return "", fmt.Errorf("CODEX_HOME: %w", err)
	}
	codexHome, err = canonicalPath(codexHome)
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return codexHome, nil
}

// resolveProtectedPaths returns the paths ebi-x always hides from the agent:
// memory and bot state, the .env file holding the Slack tokens, the local
// Codex config, and the operator's extra denied paths. The Codex home is
// handled by the runner, which lists it on every turn.
func resolveProtectedPaths(home, memoryDir, stateDir, extraDenied string) ([]string, error) {
	candidates := []string{
		memoryDir,
		stateDir,
		".env",
		filepath.Join(home, ".env"),
		filepath.Join(home, ".codex"),
	}
	for _, item := range splitList(extraDenied) {
		expanded, err := expandHome(item)
		if err != nil {
			return nil, fmt.Errorf("EBIX_DENIED_READ_PATHS %q: %w", item, err)
		}
		candidates = append(candidates, expanded)
	}
	var paths []string
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		path, err := canonicalPath(candidate)
		if err != nil {
			return nil, fmt.Errorf("resolve protected path %q: %w", candidate, err)
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

// validateIsolation keeps the agent's workspace and every writable root
// disjoint from protected paths. Codex receives only the current scoped
// memory through its prompt, and a writable .codex/config.toml, for example,
// would let the agent register commands that Codex runs outside the sandbox.
func validateIsolation(writableRoots, protectedPaths []string) error {
	for _, root := range writableRoots {
		root, err := canonicalPath(root)
		if err != nil {
			return fmt.Errorf("resolve writable path %q: %w", root, err)
		}
		for _, protected := range protectedPaths {
			if pathsOverlap(root, protected) {
				return fmt.Errorf("writable path %q overlaps protected path %q", root, protected)
			}
		}
	}
	return nil
}

// positiveIntEnv reads an integer of at least 1 from the environment
// variable name, returning fallback when it is unset or blank.
func positiveIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", name, value, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s %q: %w", name, value, errors.New("must be at least 1"))
	}
	return n, nil
}

// validWorkflowID reports whether id looks like a Slack workflow ID.
func validWorkflowID(id string) bool {
	if len(id) <= 2 || !strings.HasPrefix(id, "Wf") {
		return false
	}
	for _, char := range id[2:] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
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
