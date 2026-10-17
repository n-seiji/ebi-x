package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCopyCopiesOnlyReadableScopes(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(t.TempDir(), CopyDirName)
	for _, entry := range []struct {
		scope   Scope
		channel string
		text    string
	}{
		{ScopeGlobal, "C1", "global fact"},
		{ScopeChannel, "C1", "own channel fact"},
		{ScopeChannel, "C2", "other channel fact"},
	} {
		if _, err := AppendScoped(root, entry.scope, entry.channel, entry.text, testNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteCopy(root, "C1", dest); err != nil {
		t.Fatalf("WriteCopy() error = %v", err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 2 {
		t.Fatalf("copy has %d files, want global and channel", len(entries))
	}
	if data, _ := os.ReadFile(filepath.Join(dest, ChannelCopyName)); string(data) != "\n### 2026-10-15\nown channel fact\n" {
		t.Fatalf("channel copy = %q", data)
	}

	// A channel without memory loses a stale copy.
	if err := WriteCopy(root, "C3", dest); err != nil {
		t.Fatalf("WriteCopy() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ChannelCopyName)); !os.IsNotExist(err) {
		t.Fatalf("stale channel copy remains, stat err = %v", err)
	}
}
