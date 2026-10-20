//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const lockFilename = "ebi-x.lock"

// LockDir takes an exclusive lock on dir so a second ebi-x process sharing
// the same data cannot overwrite this one's state. The lock lasts until the
// returned release is called or the process exits. The holder's PID is kept
// in the file so a conflict names the process to stop.
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
			return nil, fmt.Errorf("lock %q: %w", path, errors.New("another ebi-x process"+holderPID(path)+" is using this data directory"))
		}
		return nil, fmt.Errorf("lock %q: %w", path, err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() { _ = file.Close() }, nil
}

// holderPID returns " (pid N)" for the PID the holder wrote to path, or ""
// when it cannot be read.
func holderPID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}
