//go:build !unix

package state

// flock is unavailable here; run one ebi-x per data directory.
func LockDir(string) (func(), error) {
	return func() {}, nil
}
