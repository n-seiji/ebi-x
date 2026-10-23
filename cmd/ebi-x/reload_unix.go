//go:build unix

package main

import (
	"os"
	"syscall"
)

// reloadSignal asks ebi-x to finish its running work and restart itself
// from the binary at its own path.
var reloadSignal os.Signal = syscall.SIGHUP

// reexec replaces this process with the binary at path, keeping the PID and
// arguments.
func reexec(path string, env []string) error {
	return syscall.Exec(path, os.Args, env)
}
