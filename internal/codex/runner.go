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
	"os/exec"
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
)

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
	// DeveloperInstructions is injected into each session with the
	// "developer" role, which outranks user messages and AGENTS.md in the
	// model's chain of command. Codex stores it once per thread, so passing
	// it again on resume does not duplicate it.
	DeveloperInstructions string
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
	writableRoots []string,
	prompt string,
	onThreadStarted func(id string) error,
) (*TurnResult, error) {
	configOverrides, err := loadConfigOverrides(r.ConfigPath)
	if err != nil {
		return nil, err
	}
	args := buildArgsWithOverrides(threadID, sandbox, cwd, writableRoots, r.DeniedReadPaths, r.Model, r.DeveloperInstructions, configOverrides)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.Command, args...)
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
	parseErr := parseJSONL(io.LimitReader(stdout, maxStdoutBytes+1), result, onThreadStarted)
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
			return nil, fmt.Errorf("codex exec: %s: %w", text, waitErr)
		}
		return nil, fmt.Errorf("codex exec: %w", waitErr)
	}

	truncateFinalMessage(result)
	return result, nil
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
	args = append(args, "-c", `approval_policy="never"`, "-c", `default_permissions="ebiii"`)
	parentProfile := ":read-only"
	if sandbox == "workspace-write" {
		parentProfile = ":workspace"
	}
	args = append(args, "-c", "permissions.ebiii.extends="+strconv.Quote(parentProfile))
	if len(deniedReadPaths) > 0 {
		entries := make([]string, 0, len(deniedReadPaths))
		for _, path := range deniedReadPaths {
			entries = append(entries, strconv.Quote(path)+`="deny"`)
		}
		args = append(args, "-c", "permissions.ebiii.filesystem={"+strings.Join(entries, ",")+"}")
	}
	if developerInstructions != "" {
		args = append(args, "-c", "developer_instructions="+strconv.Quote(developerInstructions))
	}
	if sandbox == "workspace-write" || sandbox == "read-only-network" {
		args = append(args, "-c", "permissions.ebiii.network.enabled=true")
	}
	if sandbox == "workspace-write" {
		for _, root := range writableRoots {
			args = append(args, "--add-dir", root)
		}
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
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type eventErr struct {
	Message string `json:"message"`
}

func parseJSONL(r io.Reader, result *TurnResult, onThreadStarted func(string) error) error {
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
		case "item.completed":
			if ev.Item == nil || ev.Item.Type != "agent_message" {
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
