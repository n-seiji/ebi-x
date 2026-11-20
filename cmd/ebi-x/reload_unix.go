//go:build unix

package main

import (
	"os"
	"syscall"
)

var reloadSignal os.Signal = syscall.SIGHUP

func reexec(path string, env []string) error {
	return syscall.Exec(path, os.Args, env)
}
