package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestReadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Read(t.TempDir(), "")
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
	got, err := Read(dir, "")
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
	got, err := Read(dir, "")
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

func TestReadContextReadsOnlyGlobalAndChannelScopes(t *testing.T) {
	root := t.TempDir()
	entries := []struct {
		scope Scope
		text  string
	}{
		{scope: ScopeGlobal, text: "global fact"},
		{scope: ScopeChannel, text: "channel convention"},
	}
	for _, entry := range entries {
		if _, err := AppendScoped(root, entry.scope, "C456", entry.text, testNow); err != nil {
			t.Fatalf("AppendScoped(%s) error = %v", entry.scope, err)
		}
	}

	got, err := ReadContext(root, "C456", "")
	if err != nil {
		t.Fatalf("ReadContext() error = %v", err)
	}
	if got.Global != "### 2026-10-15\nglobal fact" || got.Channel != "### 2026-10-15\nchannel convention" {
		t.Fatalf("ReadContext() = %#v", got)
	}
	other, err := ReadContext(root, "C999", "")
	if err != nil {
		t.Fatalf("ReadContext(other) error = %v", err)
	}
	if other.Global != "### 2026-10-15\nglobal fact" || other.Channel != "" {
		t.Fatalf("ReadContext(other) = %#v", other)
	}
}

func TestScopeDirRejectsUnsafeIDs(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		id    string
	}{
		{name: "channel traversal", scope: ScopeChannel, id: "C/other"},
		{name: "unknown scope", scope: Scope("other")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ScopeDir(t.TempDir(), test.scope, test.id); err == nil {
				t.Fatal("ScopeDir() error = nil, want validation error")
			}
		})
	}
}

func TestReadContextReturnsOtherScopesWhenOneFails(t *testing.T) {
	root := t.TempDir()
	if _, err := AppendScoped(root, ScopeGlobal, "C1", "global fact", testNow); err != nil {
		t.Fatalf("append global: %v", err)
	}
	got, err := ReadContext(root, "unsafe/channel", "")
	if err == nil {
		t.Fatalf("ReadContext() error = %v, want validation error", err)
	}
	if got.Global != "### 2026-10-15\nglobal fact" {
		t.Fatalf("ReadContext() global = %q", got.Global)
	}
}

func TestAppendCreatesAndAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	first, err := Append(dir, "一つ目の学び\n", testNow)
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if first != "一つ目の学び" {
		t.Fatalf("Append() = %q", first)
	}
	if _, err := Append(dir, "二つ目の学び", testNow); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	want := "\n### 2026-10-15\n一つ目の学び\n\n### 2026-10-15\n二つ目の学び\n"
	if string(data) != want {
		t.Fatalf("memory file = %q, want %q", data, want)
	}
}

func TestAppendSkipsEmptyEntry(t *testing.T) {
	dir := t.TempDir()
	written, err := Append(dir, "  \n ", testNow)
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
	written, err := Append(dir, strings.Repeat("界", MaxAppendBytes), testNow)
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

var testNow = time.Date(2026, 10, 15, 21, 0, 0, 0, time.Local)

func TestExceedsPromptLimit(t *testing.T) {
	root := t.TempDir()
	over, err := ExceedsPromptLimit(root, ScopeChannel, "C1")
	if err != nil || over {
		t.Fatalf("ExceedsPromptLimit() on missing file = %v, %v; want false, nil", over, err)
	}
	if _, err := AppendScoped(root, ScopeChannel, "C1", "short fact", testNow); err != nil {
		t.Fatalf("AppendScoped() error = %v", err)
	}
	if over, err := ExceedsPromptLimit(root, ScopeChannel, "C1"); err != nil || over {
		t.Fatalf("ExceedsPromptLimit() on small file = %v, %v; want false, nil", over, err)
	}
	for range 3 {
		if _, err := AppendScoped(root, ScopeChannel, "C1", strings.Repeat("a", MaxAppendBytes), testNow); err != nil {
			t.Fatalf("AppendScoped() error = %v", err)
		}
	}
	if over, err := ExceedsPromptLimit(root, ScopeChannel, "C1"); err != nil || !over {
		t.Fatalf("ExceedsPromptLimit() on large file = %v, %v; want true, nil", over, err)
	}
	if over, err := ExceedsPromptLimit(root, ScopeGlobal, "C1"); err != nil || over {
		t.Fatalf("ExceedsPromptLimit() on other scope = %v, %v; want false, nil", over, err)
	}
}
