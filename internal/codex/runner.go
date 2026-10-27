// Package codex runs Codex CLI turns and parses their JSONL output.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxStdoutBytes = 64 << 20
	maxStderrBytes = 16 << 20
	maxMessages    = 100
	maxFinalBytes  = 128 << 10
	// maxArgBytes keeps each argument below Linux's 128KiB MAX_ARG_STRLEN.
	// The filesystem list is one argument because Codex cannot parse paths as
	// separate dotted keys.
	maxArgBytes = 120 << 10
)

// envAllowNames and envAllowPrefixes are the only variables the Codex child
// inherits; the rest of the bot's environment (.env contents, the operator's
// shell) is withheld so a prompt-injected turn cannot read it back. Everything
// here is non-secret configuration except the two Codex API keys, which the
// shell environment policy (see shellEnvPolicyOverrides) keeps out of the
// commands Codex runs. SSH_AUTH_SOCK is deliberately absent: the config layer
// adds it through PassEnv only when a push or signature needs ~/.ssh.
var envAllowNames = map[string]struct{}{
	"HOME": {}, "PATH": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"LANG": {}, "TZ": {}, "TERM": {}, "TMPDIR": {}, "CODEX_HOME": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "no_proxy": {}, "all_proxy": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	// Codex authenticates with these.
	"OPENAI_API_KEY": {}, "CODEX_API_KEY": {},
	// Endpoint and certificate configuration.
	"OPENAI_BASE_URL": {}, "CODEX_CA_CERTIFICATE": {}, "NODE_EXTRA_CA_CERTS": {},
	"REQUESTS_CA_BUNDLE": {}, "CURL_CA_BUNDLE": {}, "GIT_SSL_CAINFO": {},
	// git configuration.
	"GIT_SSH_COMMAND": {}, "GIT_CONFIG_GLOBAL": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_CONFIG_NOSYSTEM": {}, "GNUPGHOME": {},
	// Toolchain locations and settings.
	"GOPATH": {}, "GOPRIVATE": {}, "GOPROXY": {}, "GONOSUMDB": {}, "GONOPROXY": {}, "GOFLAGS": {},
	"CARGO_HOME": {}, "RUSTUP_HOME": {}, "JAVA_HOME": {}, "PYENV_ROOT": {},
	"NVM_DIR": {}, "VOLTA_HOME": {}, "PNPM_HOME": {},
	// GitHub CLI location, and the desktop session bus.
	"GH_HOST": {}, "GH_CONFIG_DIR": {}, "DBUS_SESSION_BUS_ADDRESS": {},
}

var envAllowPrefixes = []string{"LC_", "XDG_", "MISE_"}

// envSecretMarkers are substrings that disqualify a name from matching an
// allowed prefix, so MISE_GITHUB_TOKEN stays out although MISE_ is allowed.
var envSecretMarkers = []string{"TOKEN", "SECRET", "PASSWORD", "KEY"}

// Runner executes Codex CLI turns.
type Runner struct {
	Command string
	Model   string
	// ConfigPath is a local project config whose values are forwarded as CLI
	// overrides while the global user config remains disabled.
	ConfigPath string
	// DeniedReadPaths are protected backing stores that model-generated
	// commands must not read or write. They are enforced with a Codex
	// permission profile, independently of prompt instructions.
	DeniedReadPaths []string
	// PassEnv lists extra environment variable names Codex inherits beyond
	// the built-in allowlist.
	PassEnv []string
	// CodexHome holds Codex credentials, configuration, and the history of
	// every thread. Each turn denies everything in it except codexHomeVisible.
	CodexHome string
	// DeveloperInstructions is injected into each session with the
	// "developer" role, which outranks user messages and AGENTS.md in the
	// model's chain of command. Codex stores it once per thread, so passing
	// it again on resume does not duplicate it.
	DeveloperInstructions string
	// GitHub, when set, gives each turn its own short-lived GitHub
	// credential instead of the host's git and gh configuration.
	GitHub TurnCredentials
}

// TurnCredentials prepares a credential for one turn: the variables that
// point its commands at it and a function that removes it.
type TurnCredentials interface {
	TurnEnv(ctx context.Context) ([]string, func(), error)
}

// TurnResult is the outcome reported by one Codex CLI turn.
type TurnResult struct {
	ThreadID  string
	Messages  []string
	Completed bool
	Err       string
}

// Run executes a new or resumed Codex CLI turn.
func (r *Runner) Run(
	ctx context.Context,
	threadID, sandbox, cwd string,
	writableRoots, deniedPaths []string,
	prompt string,
	onThreadStarted func(id string) error,
	onActivity func(Activity),
) (*TurnResult, error) {
	configOverrides, err := loadConfigOverrides(r.ConfigPath)
	if err != nil {
		return nil, err
	}
	homeDenied, err := codexHomeDenied(r.CodexHome)
	if err != nil {
		return nil, err
	}
	denied := append(append(append([]string(nil), r.DeniedReadPaths...), homeDenied...), deniedPaths...)
	args := buildArgsWithOverrides(threadID, sandbox, cwd, writableRoots, denied, r.Model, r.DeveloperInstructions, configOverrides)
	for _, arg := range args {
		if len(arg) > maxArgBytes {
			// Running with a truncated deny list would expose protected paths.
			return nil, fmt.Errorf("codex exec: argument of %d bytes exceeds %d; remove unused thread directories under data/workspace", len(arg), maxArgBytes)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.Command, args...)
	cmd.Env = FilterEnv(os.Environ(), r.PassEnv)
	if r.GitHub != nil {
		// TurnEnv has already retried transient failures. Without the turn
		// credential, git and gh fall back to the host's configuration,
		// so a failure does not stop the turn.
		env, cleanup, err := r.GitHub.TurnEnv(ctx)
		if err != nil {
			log.Printf("codex exec: prepare the turn's GitHub credential: %v", err)
		} else {
			defer cleanup()
			cmd.Env = append(cmd.Env, env...)
		}
	}
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex exec: stdout pipe: %w", err)
	}
	stderr := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex exec: start: %w", err)
	}

	result := &TurnResult{}
	parseErr := parseJSONL(io.LimitReader(stdout, maxStdoutBytes+1), result, onThreadStarted, onActivity)
	if parseErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil, fmt.Errorf("codex exec: %w", ctx.Err())
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		if result.Err != "" {
			truncateFinalMessage(result)
			return result, nil
		}
		if text := strings.TrimSpace(stderr.String()); text != "" {
			if IsNotSignedInMessage(text) {
				return nil, fmt.Errorf("codex exec: %s: %w: %w", text, ErrNotSignedIn, waitErr)
			}
			return nil, fmt.Errorf("codex exec: %s: %w", text, waitErr)
		}
		return nil, fmt.Errorf("codex exec: %w", waitErr)
	}

	truncateFinalMessage(result)
	return result, nil
}

// codexHomeVisible are the Codex home entries sandboxed commands must still
// read: skills and plugins the model opens itself, the helper binaries Codex
// puts on PATH, installed Codex packages, and the shell snapshots it sources.
var codexHomeVisible = map[string]struct{}{
	"skills":          {},
	"plugins":         {},
	"packages":        {},
	"tmp":             {},
	".tmp":            {},
	"shell_snapshots": {},
}

// codexHomeDenied lists the entries of home to hide. Codex adds state files
// across versions, so everything not known to be needed is denied rather
// than naming the sensitive ones.
func codexHomeDenied(home string) ([]string, error) {
	if home == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex exec: list Codex home: %w", err)
	}
	var denied []string
	for _, entry := range entries {
		if _, visible := codexHomeVisible[entry.Name()]; !visible {
			denied = append(denied, filepath.Join(home, entry.Name()))
		}
	}
	return denied, nil
}

// FilterEnv returns the entries of env that Codex may inherit: the built-in
// allowlist plus the names in extra.
func FilterEnv(env, extra []string) []string {
	kept, _ := splitEnv(env, extra)
	return kept
}

// WithheldEnvNames returns the sorted names in env that FilterEnv drops.
func WithheldEnvNames(env, extra []string) []string {
	_, withheld := splitEnv(env, extra)
	slices.Sort(withheld)
	return withheld
}

func splitEnv(env, extra []string) (kept, withheld []string) {
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if envAllowed(name, extra) {
			kept = append(kept, entry)
		} else {
			withheld = append(withheld, name)
		}
	}
	return kept, withheld
}

func envAllowed(name string, extra []string) bool {
	if _, ok := envAllowNames[name]; ok {
		return true
	}
	if slices.Contains(extra, name) {
		return true
	}
	upper := strings.ToUpper(name)
	for _, marker := range envSecretMarkers {
		if strings.Contains(upper, marker) {
			return false
		}
	}
	for _, prefix := range envAllowPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// shellEnvPolicyOverrides pins Codex's shell environment policy, which
// decides what the commands it runs inherit from its own environment. Codex
// keeps its default excludes (names containing KEY, SECRET, or TOKEN) and
// additionally drops the two API keys it authenticates with by name, so
// neither depends on Codex's defaults or on local configuration.
func shellEnvPolicyOverrides() []string {
	excluded := make([]string, len(shellEnvExcluded))
	for i, name := range shellEnvExcluded {
		excluded[i] = strconv.Quote(name)
	}
	return []string{
		"-c", "shell_environment_policy.ignore_default_excludes=false",
		"-c", "shell_environment_policy.exclude=[" + strings.Join(excluded, ",") + "]",
	}
}

var shellEnvExcluded = []string{"OPENAI_API_KEY", "CODEX_API_KEY"}

// shellEnvDefaultExcludeMarkers are the substrings of Codex's default shell
// environment excludes, which ignore_default_excludes=false keeps on. They
// differ from envSecretMarkers, ebi-x's own rule for allowed prefixes.
var shellEnvDefaultExcludeMarkers = []string{"KEY", "SECRET", "TOKEN"}

// HiddenFromCommands reports whether Codex removes the variable from the
// commands it runs although Codex itself inherits it.
func HiddenFromCommands(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range shellEnvDefaultExcludeMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return slices.Contains(shellEnvExcluded, name)
}

func buildArgs(threadID, sandbox, cwd string, writableRoots, deniedReadPaths []string, model, developerInstructions string) []string {
	return buildArgsWithOverrides(threadID, sandbox, cwd, writableRoots, deniedReadPaths, model, developerInstructions, nil)
}

func buildArgsWithOverrides(threadID, sandbox, cwd string, writableRoots, deniedReadPaths []string, model, developerInstructions string, configOverrides []string) []string {
	args := []string{"exec"}
	if threadID != "" {
		args = append(args, "resume", threadID)
	}
	args = append(args,
		"--json",
		"--skip-git-repo-check",
		// A user config containing legacy sandbox_mode disables permission
		// profiles. Ignore it so the deny rules below cannot be weakened by
		// machine-local Codex settings; authentication still uses CODEX_HOME.
		"--ignore-user-config",
	)
	for _, override := range configOverrides {
		args = append(args, "-c", override)
	}
	// Security invariants come after local values so local configuration can
	// never weaken them.
	args = append(args, "-c", `approval_policy="never"`, "-c", `default_permissions="ebi-x"`)
	args = append(args, shellEnvPolicyOverrides()...)
	parentProfile := ":read-only"
	if sandbox == "workspace-write" {
		parentProfile = ":workspace"
	}
	args = append(args, "-c", "permissions.ebi-x.extends="+strconv.Quote(parentProfile))
	// Writable roots are profile entries rather than --add-dir, which
	// "codex exec resume" does not accept. A deny entry wins over a write
	// entry for the same or an enclosing path, and a path listed twice would
	// make the inline table invalid, so a denied root is left out.
	denied := make(map[string]struct{}, len(deniedReadPaths))
	entries := make([]string, 0, len(writableRoots)+len(deniedReadPaths))
	for _, path := range deniedReadPaths {
		denied[path] = struct{}{}
	}
	if sandbox == "workspace-write" {
		for _, root := range writableRoots {
			if _, ok := denied[root]; ok {
				continue
			}
			entries = append(entries, strconv.Quote(root)+`="write"`)
		}
	}
	for _, path := range deniedReadPaths {
		entries = append(entries, strconv.Quote(path)+`="deny"`)
	}
	if len(entries) > 0 {
		args = append(args, "-c", "permissions.ebi-x.filesystem={"+strings.Join(entries, ",")+"}")
	}
	if developerInstructions != "" {
		args = append(args, "-c", "developer_instructions="+strconv.Quote(developerInstructions))
	}
	if sandbox == "workspace-write" || sandbox == "read-only-network" {
		args = append(args, "-c", "permissions.ebi-x.network.enabled=true")
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	if threadID == "" {
		args = append(args, "-C", cwd)
	}
	return append(args, "-")
}

type event struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id,omitempty"`
	Message  string     `json:"message,omitempty"`
	Item     *eventItem `json:"item,omitempty"`
	Error    *eventErr  `json:"error,omitempty"`
}

type eventItem struct {
	Type    string        `json:"type"`
	Text    string        `json:"text,omitempty"`
	Command string        `json:"command,omitempty"`
	Changes []eventChange `json:"changes,omitempty"`
	Query   string        `json:"query,omitempty"`
	Server  string        `json:"server,omitempty"`
	Tool    string        `json:"tool,omitempty"`
	Items   []eventTodo   `json:"items,omitempty"`
}

type eventChange struct {
	Path string `json:"path"`
}

type eventTodo struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type eventErr struct {
	Message string `json:"message"`
}

func parseJSONL(r io.Reader, result *TurnResult, onThreadStarted func(string) error, onActivity func(Activity)) error {
	counted := &countingReader{reader: r}
	scanner := bufio.NewScanner(counted)
	scanner.Buffer(make([]byte, 64*1024), maxStdoutBytes+1)
	seenCompleted := false
	seenFailed := false

	for scanner.Scan() {
		if counted.count > maxStdoutBytes {
			return errors.New("parse codex jsonl: stdout exceeds 64MB")
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("parse codex jsonl: %w", err)
		}
		switch ev.Type {
		case "thread.started":
			result.ThreadID = ev.ThreadID
			if onThreadStarted != nil {
				if err := onThreadStarted(ev.ThreadID); err != nil {
					return fmt.Errorf("codex thread started callback: %w", err)
				}
			}
		case "item.started", "item.updated", "item.completed":
			if onActivity != nil && ev.Item != nil {
				if activity, ok := activityFromItem(ev.Type, ev.Item); ok {
					onActivity(activity)
				}
			}
			if ev.Type != "item.completed" || ev.Item == nil || ev.Item.Type != "agent_message" {
				continue
			}
			if len(result.Messages) >= maxMessages {
				return errors.New("parse codex jsonl: agent_message count exceeds 100")
			}
			result.Messages = append(result.Messages, ev.Item.Text)
		case "turn.completed":
			seenCompleted = true
		case "turn.failed":
			seenFailed = true
			if ev.Error != nil {
				result.Err = ev.Error.Message
			}
		case "error":
			seenFailed = true
			result.Err = ev.Message
		}
	}
	if err := scanner.Err(); err != nil {
		if counted.count > maxStdoutBytes {
			return errors.New("parse codex jsonl: stdout exceeds 64MB")
		}
		return fmt.Errorf("parse codex jsonl: %w", err)
	}
	if counted.count > maxStdoutBytes {
		return errors.New("parse codex jsonl: stdout exceeds 64MB")
	}
	result.Completed = seenCompleted && !seenFailed
	return nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func truncateFinalMessage(result *TurnResult) {
	if len(result.Messages) == 0 {
		return
	}
	last := len(result.Messages) - 1
	text := result.Messages[last]
	if len(text) <= maxFinalBytes {
		return
	}
	end := maxFinalBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	result.Messages[last] = text[:end]
}

type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
