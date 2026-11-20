package playbook

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxNoteBytes       = 1 << 10
	maxPromptNoteBytes = 2 << 10
)

func Find(playbooks []Playbook, name string) (Playbook, bool) {
	name = strings.TrimSpace(name)
	for _, item := range playbooks {
		if item.Name == name {
			return item, true
		}
	}
	return Playbook{}, false
}

// Named after the playbook file, never after anything the model wrote.
func notesPath(dir string, item Playbook) string {
	return filepath.Join(dir, strings.TrimSuffix(filepath.Base(item.Path), ".md")+".md")
}

// Callers serialize concurrent writes.
func AppendNote(dir string, item Playbook, note string, now time.Time) (written string, err error) {
	note = strings.Join(strings.Fields(note), " ")
	if note == "" {
		return "", nil
	}
	if len(note) > MaxNoteBytes {
		note = truncateRunes(note, MaxNoteBytes) + "…"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(notesPath(dir, item), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			written, err = "", closeErr
		}
	}()
	if _, err := fmt.Fprintf(file, "- %s %s\n", now.Format(time.RFC3339), note); err != nil {
		return "", err
	}
	return note, nil
}

func AttachNotes(dir string, playbooks []Playbook) error {
	var errs []error
	for i := range playbooks {
		notes, err := readNotes(notesPath(dir, playbooks[i]), playbooks[i].ModTime)
		if err != nil {
			errs = append(errs, fmt.Errorf("read notes of playbook %q: %w", playbooks[i].Name, err))
			continue
		}
		playbooks[i].Notes = notes
	}
	return errors.Join(errs...)
}

func readNotes(path string, since time.Time) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 4096), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		item, _ := strings.CutPrefix(line, "- ")
		stamp, note, _ := strings.Cut(item, " ")
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			lines = append(lines, "- "+item)
			continue
		}
		if !at.After(since) || strings.TrimSpace(note) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s", at.Format("2006-01-02"), strings.TrimSpace(note)))
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	start, size := len(lines), 0
	for start > 0 && size+len(lines[start-1])+1 <= maxPromptNoteBytes {
		start--
		size += len(lines[start]) + 1
	}
	return strings.Join(lines[start:], "\n"), nil
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
