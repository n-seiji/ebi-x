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

// Notes keep a playbook current between edits, like memory per use: a work
// turn that learns something the playbook does not say yet, or says
// differently now, adds a dated line, and later sessions see the lines
// written since the playbook file last changed. Editing the playbook to fold
// the notes in retires them. The bot writes the notes; the agent never does.
const (
	// MaxNoteBytes caps one note.
	MaxNoteBytes = 1 << 10
	// maxPromptNoteBytes caps how much of one playbook's notes is shown,
	// keeping the newest.
	maxPromptNoteBytes = 2 << 10
)

// Find returns the playbook with name.
func Find(playbooks []Playbook, name string) (Playbook, bool) {
	name = strings.TrimSpace(name)
	for _, item := range playbooks {
		if item.Name == name {
			return item, true
		}
	}
	return Playbook{}, false
}

// notesPath names a playbook's notes file after its playbook file, never
// after anything the model wrote.
func notesPath(dir string, item Playbook) string {
	return filepath.Join(dir, strings.TrimSuffix(filepath.Base(item.Path), ".md")+".md")
}

// AppendNote adds one dated note to item's notes in dir and returns the note
// as written. Callers serialize concurrent writes.
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

// AttachNotes fills in each playbook's Notes from dir: the dated lines
// written after the playbook file last changed, shown with their date, plus
// any line an operator wrote without a date. A missing notes file means no
// notes. Notes that cannot be read are reported and left empty.
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
	// Keep the newest lines that fit.
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
