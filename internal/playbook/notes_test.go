package playbook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePlaybook(t *testing.T, dir, file, name string, modTime time.Time) Playbook {
	t.Helper()
	path := filepath.Join(dir, file)
	content := "---\nname: " + name + "\ndescription: test\n---\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	playbooks, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := Find(playbooks, name)
	if !ok {
		t.Fatalf("Find(%q) found nothing in %#v", name, playbooks)
	}
	return item
}

func TestNotesShowOnlyWhatIsNewerThanThePlaybook(t *testing.T) {
	playbooksDir, notesDir := t.TempDir(), t.TempDir()
	edited := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	item := writePlaybook(t, playbooksDir, "slides.md", "slides", edited)

	for _, note := range []struct {
		at   time.Time
		text string
	}{
		{edited.Add(-time.Hour), "納品は3週後"},
		{edited.Add(24 * time.Hour), "納品は2週後に\n変わった"},
	} {
		if _, err := AppendNote(notesDir, item, note.text, note.at); err != nil {
			t.Fatalf("AppendNote() error = %v", err)
		}
	}
	playbooks := []Playbook{item}
	if err := AttachNotes(notesDir, playbooks); err != nil {
		t.Fatalf("AttachNotes() error = %v", err)
	}
	if want := "- 2026-10-06 納品は2週後に 変わった"; playbooks[0].Notes != want {
		t.Errorf("Notes = %q, want %q", playbooks[0].Notes, want)
	}
	if _, err := os.Stat(filepath.Join(notesDir, "slides.md")); err != nil {
		t.Errorf("notes file is not named after the playbook file: %v", err)
	}
}

func TestNotesKeepOperatorLinesAndTheNewest(t *testing.T) {
	playbooksDir, notesDir := t.TempDir(), t.TempDir()
	edited := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	item := writePlaybook(t, playbooksDir, "deploy.md", "deploy", edited)

	content := "- 手で書いたメモ\n"
	for i := range 100 {
		content += "- " + edited.Add(time.Duration(i+1)*time.Hour).Format(time.RFC3339) + " " + strings.Repeat("x", 40) + "\n"
	}
	content += "- " + edited.Add(200*time.Hour).Format(time.RFC3339) + " 最新\n"
	if err := os.WriteFile(filepath.Join(notesDir, "deploy.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	playbooks := []Playbook{item}
	if err := AttachNotes(notesDir, playbooks); err != nil {
		t.Fatalf("AttachNotes() error = %v", err)
	}
	notes := playbooks[0].Notes
	if len(notes) > maxPromptNoteBytes {
		t.Errorf("Notes is %d bytes, want at most %d", len(notes), maxPromptNoteBytes)
	}
	if !strings.HasSuffix(notes, "最新") {
		t.Errorf("Notes does not end with the newest note: %q", notes)
	}
}

func TestAttachNotesWithoutNotes(t *testing.T) {
	item := writePlaybook(t, t.TempDir(), "a.md", "a", time.Now())
	playbooks := []Playbook{item}
	if err := AttachNotes(filepath.Join(t.TempDir(), "missing"), playbooks); err != nil {
		t.Fatalf("AttachNotes() error = %v", err)
	}
	if playbooks[0].Notes != "" {
		t.Errorf("Notes = %q, want empty", playbooks[0].Notes)
	}
}

func TestAppendNoteCapsLength(t *testing.T) {
	item := writePlaybook(t, t.TempDir(), "a.md", "a", time.Now())
	written, err := AppendNote(t.TempDir(), item, strings.Repeat("あ", MaxNoteBytes), time.Now())
	if err != nil {
		t.Fatalf("AppendNote() error = %v", err)
	}
	if len(written) > MaxNoteBytes+len("…") || !strings.HasSuffix(written, "…") {
		t.Errorf("AppendNote() wrote %d bytes, want the note cut to %d", len(written), MaxNoteBytes)
	}
}
