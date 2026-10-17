package memory

import (
	"errors"
	"os"
	"path/filepath"
)

// CopyDirName is the directory in a thread's workspace that holds the
// thread's read copy of its memory.
const CopyDirName = ".ebi-x-memory"

// Copy file names inside CopyDirName.
const (
	GlobalCopyName  = "global.md"
	ChannelCopyName = "channel.md"
)

// WriteCopy puts the full global memory and the memory of channelID into
// dest, so a turn can search all of it, as Codex and Claude Code let their
// agents grep a memory file, instead of reading only what fits in the prompt.
// Only the scopes the thread may already read are copied; a scope with no
// memory has its copy removed. The copy is refreshed on every turn and is
// never read back: memory changes still go through the bot.
func WriteCopy(root, channelID, dest string) error {
	var errs []error
	for _, target := range []struct {
		scope Scope
		id    string
		name  string
	}{
		{scope: ScopeGlobal, name: GlobalCopyName},
		{scope: ScopeChannel, id: channelID, name: ChannelCopyName},
	} {
		dir, err := ScopeDir(root, target.scope, target.id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		path := filepath.Join(dest, target.name)
		data, err := os.ReadFile(filepath.Join(dir, FileName))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(dest, 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
