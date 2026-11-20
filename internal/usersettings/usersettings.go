package usersettings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Not MEMORY.md, so retired user memory never comes back as settings.
const FileName = "settings.md"

const (
	MaxLines     = 10
	MaxLineBytes = 200
)

func Dir(root, userID string) (string, error) {
	if !ValidUserID(userID) {
		return "", fmt.Errorf("invalid Slack user ID %q", userID)
	}
	return filepath.Join(root, "users", userID), nil
}

func Read(root, userID string) ([]string, error) {
	dir, err := Dir(root, userID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settings []string
	for _, line := range strings.Split(string(data), "\n") {
		line, _ = strings.CutPrefix(strings.TrimSpace(line), "- ")
		if line = strings.TrimSpace(line); line != "" {
			settings = append(settings, line)
		}
	}
	return Normalize(settings), nil
}

// Callers serialize concurrent writes.
func Write(root, userID string, settings []string) ([]string, error) {
	dir, err := Dir(root, userID)
	if err != nil {
		return nil, err
	}
	settings = Normalize(settings)
	path := filepath.Join(dir, FileName)
	if len(settings) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var builder strings.Builder
	for _, setting := range settings {
		builder.WriteString("- " + setting + "\n")
	}
	// Write a temporary file and rename it, so a reader never sees half of
	// the settings.
	file, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(builder.String()); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	return settings, nil
}

func Normalize(settings []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, setting := range settings {
		setting = strings.Join(strings.Fields(setting), " ")
		if len(setting) > MaxLineBytes {
			setting = truncateRunes(setting, MaxLineBytes) + "…"
		}
		if setting == "" || seen[setting] {
			continue
		}
		seen[setting] = true
		result = append(result, setting)
		if len(result) == MaxLines {
			break
		}
	}
	return result
}

func truncateRunes(text string, limit int) string {
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func ValidUserID(id string) bool {
	if len(id) < 2 || (id[0] != 'U' && id[0] != 'W') {
		return false
	}
	for i := 1; i < len(id); i++ {
		if (id[i] < 'A' || id[i] > 'Z') && (id[i] < '0' || id[i] > '9') {
			return false
		}
	}
	return true
}
