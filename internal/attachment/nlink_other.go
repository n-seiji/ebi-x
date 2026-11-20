//go:build !unix

package attachment

import "io/fs"

func linkCount(fs.FileInfo) uint64 { return 1 }
