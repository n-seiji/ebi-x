//go:build unix

package attachment

import (
	"io/fs"
	"syscall"
)

// linkCount returns the number of hard links to the file.
func linkCount(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}
