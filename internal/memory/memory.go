package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const FileName = "MEMORY.md"

const (
	maxPromptBytes = 8 << 10
	MaxAppendBytes = 4 << 10
)

// Channel memory was retired; its files under channels/ are no longer read.
type Context struct {
	Global string
}

func ReadContext(root string) (Context, error) {
	value, err := Read(root)
	if err != nil {
		return Context{}, fmt.Errorf("read global memory: %w", err)
	}
	return Context{Global: value}, nil
}

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

// Append does not serialize concurrent callers; the bot holds its memory lock
// around the call. When Close fails the entry may already be on disk, but the
// write is never retried.
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
