// Package memory reads and appends global and channel MEMORY.md files.
// The Codex agent never writes these files directly; the bot validates
// proposed entries and routes them from authenticated Slack event IDs.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// FileName is the memory file managed inside the memory directory.
const FileName = "MEMORY.md"

const (
	// maxPromptBytes caps how much memory content is injected into prompts.
	maxPromptBytes = 8 << 10
	// MaxAppendBytes caps the size of one appended entry.
	MaxAppendBytes = 4 << 10
)

// Scope identifies who may reuse a memory entry.
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeChannel Scope = "channel"
)

// Context contains the memory visible to one Slack request.
type Context struct {
	Global  string
	Channel string
}

// ReadContext reads global memory plus the memory belonging to the current
// Slack channel. Successfully read scopes are returned even when a different
// scope fails.
func ReadContext(root, channelID string) (Context, error) {
	var result Context
	var errs []error
	for _, target := range []struct {
		scope Scope
		id    string
		set   func(string)
	}{
		{scope: ScopeGlobal, set: func(value string) { result.Global = value }},
		{scope: ScopeChannel, id: channelID, set: func(value string) { result.Channel = value }},
	} {
		dir, err := ScopeDir(root, target.scope, target.id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		value, err := Read(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s memory: %w", target.scope, err))
			continue
		}
		target.set(value)
	}
	return result, errors.Join(errs...)
}

// AppendScoped appends to the scope belonging to the current Slack request.
// IDs are never supplied by the model; the bot passes the authenticated event
// channel ID so an output cannot select another scope's directory.
func AppendScoped(root string, scope Scope, channelID, entry string) (string, error) {
	id := ""
	switch scope {
	case ScopeChannel:
		id = channelID
	}
	dir, err := ScopeDir(root, scope, id)
	if err != nil {
		return "", err
	}
	return Append(dir, entry)
}

// ScopeDir resolves a memory scope to a directory below root.
func ScopeDir(root string, scope Scope, id string) (string, error) {
	switch scope {
	case ScopeGlobal:
		return root, nil
	case ScopeChannel:
		if !ValidChannelID(id) {
			return "", fmt.Errorf("invalid Slack channel ID %q", id)
		}
		return filepath.Join(root, "channels", id), nil
	default:
		return "", fmt.Errorf("invalid memory scope %q", scope)
	}
}

// Read returns the memory file content capped at maxPromptBytes. A missing
// file yields an empty string without error.
func Read(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	text := string(data)
	if len(text) > maxPromptBytes {
		text = "(前半はサイズ上限のため省略)\n" + truncateTailRunes(text, maxPromptBytes)
	}
	return strings.TrimSpace(text), nil
}

// Append validates and appends one entry to the memory file, creating the
// directory and file when needed. It returns the entry actually written.
//
// Append does not serialize concurrent callers; callers must do so themselves
// (the Slack bot holds its memory lock around the call). When Close reports an
// error the entry may already be on disk, but Append still returns the error
// and an empty entry; it never retries the write on its own.
func Append(dir, entry string) (written string, err error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", nil
	}
	if len(entry) > MaxAppendBytes {
		entry = strings.TrimSpace(truncateRunes(entry, MaxAppendBytes)) + "\n(サイズ上限のため切り詰め)"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			written, err = "", closeErr
		}
	}()
	if _, err := file.WriteString("\n" + entry + "\n"); err != nil {
		return "", err
	}
	return entry, nil
}

func truncateRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func truncateTailRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

// ValidChannelID reports whether id looks like a Slack conversation ID
// (public channel, private channel, or DM).
func ValidChannelID(id string) bool {
	return validSlackID(id, "CGD")
}

func validSlackID(id, prefixes string) bool {
	if len(id) < 2 || !strings.ContainsRune(prefixes, rune(id[0])) {
		return false
	}
	for i := 1; i < len(id); i++ {
		if (id[i] < 'A' || id[i] > 'Z') && (id[i] < '0' || id[i] > '9') {
			return false
		}
	}
	return true
}
