//go:build !unix

package state

// LockDir is a no-op where flock is unavailable; run one ebi-x per data
// directory.
func LockDir(string) (func(), error) {
	return func() {}, nil
}
