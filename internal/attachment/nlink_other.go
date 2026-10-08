//go:build !unix

package attachment

import "io/fs"

// linkCount reports a single link where the platform does not expose it.
func linkCount(fs.FileInfo) uint64 { return 1 }
