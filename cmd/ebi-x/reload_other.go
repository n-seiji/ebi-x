//go:build !unix

package main

import (
	"errors"
	"os"
)

// reloadSignal is nil where a process cannot replace itself.
var reloadSignal os.Signal

func reexec(string, []string) error {
	return errors.New("restarting in place is not supported on this platform")
}
