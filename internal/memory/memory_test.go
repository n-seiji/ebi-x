package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got != "" {
		t.Fatalf("Read() = %q, want empty", got)
	}
}

func TestReadReturnsTrimmedContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("# Memory\n学び\n\n"), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got != "# Memory\n学び" {
		t.Fatalf("Read() = %q", got)
	}
}

func TestReadCapsOversizedContent(t *testing.T) {
	dir := t.TempDir()
	content := "old-entry\n" + strings.Repeat("界", maxPromptBytes) + "\nnew-entry"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(got) > maxPromptBytes+64 {
		t.Fatalf("Read() length = %d, want capped near %d", len(got), maxPromptBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("Read() returned invalid UTF-8")
	}
	if !strings.Contains(got, "省略") {
		t.Error("Read() missing truncation marker")
	}
	if strings.Contains(got, "old-entry") || !strings.Contains(got, "new-entry") {
		t.Fatalf("Read() = %q, want newest entries only", got)
	}
}

func TestReadContextIgnoresRetiredChannelMemory(t *testing.T) {
	root := t.TempDir()
	if _, err := Append(root, "global fact"); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(filepath.Join(root, "channels", "C456"), "channel convention"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadContext(root)
	if err != nil {
		t.Fatalf("ReadContext() error = %v", err)
	}
	if got != (Context{Global: "global fact"}) {
		t.Fatalf("ReadContext() = %#v, want global memory only", got)
	}
}

func TestAppendCreatesAndAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	first, err := Append(dir, "一つ目の学び\n")
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if first != "一つ目の学び" {
		t.Fatalf("Append() = %q", first)
	}
	if _, err := Append(dir, "二つ目の学び"); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	want := "\n一つ目の学び\n\n二つ目の学び\n"
	if string(data) != want {
		t.Fatalf("memory file = %q, want %q", data, want)
	}
}

func TestAppendSkipsEmptyEntry(t *testing.T) {
	dir := t.TempDir()
	written, err := Append(dir, "  \n ")
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if written != "" {
		t.Fatalf("Append() = %q, want empty", written)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("memory file should not exist, stat err = %v", err)
	}
}

func TestAppendTruncatesOversizedEntry(t *testing.T) {
	dir := t.TempDir()
	written, err := Append(dir, strings.Repeat("界", MaxAppendBytes))
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if len(written) > MaxAppendBytes+64 {
		t.Fatalf("Append() length = %d, want capped near %d", len(written), MaxAppendBytes)
	}
	if !utf8.ValidString(written) {
		t.Fatal("Append() wrote invalid UTF-8")
	}
	if !strings.Contains(written, "切り詰め") {
		t.Error("Append() missing truncation marker")
	}
}
