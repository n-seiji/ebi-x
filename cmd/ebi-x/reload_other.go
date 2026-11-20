//go:build !unix

package main

import (
	"errors"
	"os"
)

var reloadSignal os.Signal

func reexec(string, []string) error {
	return errors.New("restarting in place is not supported on this platform")
}
