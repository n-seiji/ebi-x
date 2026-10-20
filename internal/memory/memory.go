// Package memory reads and appends the global MEMORY.md file.
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

// Context contains the memory visible to one Slack request. Only global
// memory remains: lessons about a kind of work belong in its playbook, and a
// thread's own history lives in its session. Channel memory was retired and
// its files under channels/ are no longer read.
type Context struct {
	Global string
}

// ReadContext reads the global memory below root.
func ReadContext(root string) (Context, error) {
	value, err := Read(root)
	if err != nil {
		return Context{}, fmt.Errorf("read global memory: %w", err)
	}
	return Context{Global: value}, nil
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
