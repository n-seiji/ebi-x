//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockFilename = "ebi-x.lock"

// LockDir takes an exclusive lock on dir so a second ebi-x process sharing
// the same data cannot overwrite this one's state. The lock lasts until the
// returned release is called or the process exits.
func LockDir(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, lockFilename)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock %q: %w", path, errors.New("another ebi-x process is using this data directory"))
		}
		return nil, fmt.Errorf("lock %q: %w", path, err)
	}
	return func() { _ = file.Close() }, nil
}
