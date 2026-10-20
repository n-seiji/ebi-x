package usersettings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadAndRemove(t *testing.T) {
	root := t.TempDir()
	written, err := Write(root, "U1", []string{" 返事は\n短く ", "", "返事は 短く", "英語で答える"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, "|") != "返事は 短く|英語で答える" {
		t.Fatalf("written = %q", written)
	}
	path := filepath.Join(root, "users", "U1", FileName)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file = %v, %v, want mode 0600", info, err)
	}
	read, err := Read(root, "U1")
	if err != nil || strings.Join(read, "|") != strings.Join(written, "|") {
		t.Fatalf("read = %q, %v", read, err)
	}
	if _, err := Write(root, "U1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat after clearing = %v, want not exist", err)
	}
}

func TestReadIgnoresRetiredUserMemory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "users", "U1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("old private memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if settings, err := Read(root, "U1"); err != nil || len(settings) != 0 {
		t.Fatalf("settings = %q, %v, want none", settings, err)
	}
}

func TestLimitsAndUserIDs(t *testing.T) {
	var many []string
	for i := 0; i < MaxLines+5; i++ {
		many = append(many, strings.Repeat("あ", i+1))
	}
	if got := Normalize(many); len(got) != MaxLines {
		t.Fatalf("settings = %d, want %d", len(got), MaxLines)
	}
	if got := Normalize([]string{strings.Repeat("あ", MaxLineBytes)}); len(got[0]) > MaxLineBytes+len("…") {
		t.Fatalf("setting length = %d, want at most %d", len(got[0]), MaxLineBytes)
	}
	for _, id := range []string{"", "U", "C123", "U1/../x", "u1"} {
		if _, err := Dir(t.TempDir(), id); err == nil {
			t.Errorf("Dir(%q) accepted an invalid user ID", id)
		}
	}
}
