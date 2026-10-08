// Package workspace prepares per-thread working directories and git
// checkouts so work turns for different Slack threads can run in parallel
// without touching each other's files.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/memory"
)

const (
	// branchPrefix names the branches created in thread checkouts.
	branchPrefix = "ebi-x/"
	// lastUsedFile records, by its modification time, when a thread's
	// checkouts were last used by a work turn.
	lastUsedFile = ".last-used"
	// gitDirSuffix names a checkout's git directory. Codex keeps every
	// directory named .git read-only even when it is a writable root, which
	// would make commits impossible, so the git directory lives beside the
	// checkout under another name.
	gitDirSuffix = ".gitdir"
)

// Checkout is a clone of a git repository dedicated to one Slack thread.
type Checkout struct {
	// Repo is the original repository configured as a writable root.
	Repo string
	// Path is the thread's checkout of Repo.
	Path   string
	Branch string
}

// Lease holds a thread's workspace for the duration of one work turn. It
// keeps the thread locked, so the garbage collector never removes a leased
// thread's checkouts.
type Lease struct {
	// Dir is the thread's workspace directory, used as the turn's cwd.
	Dir string
	// WritableRoots replaces each git repository among the configured
	// writable roots with its thread checkout and that checkout's git
	// directory. The original repository is not writable.
	WritableRoots []string
	Checkouts     []Checkout
	// Shared reports that Dir is used by every thread, so it does not belong
	// to this thread alone.
	Shared bool

	once    sync.Once
	release func()
}

// NewLease returns a lease that calls release once when it is released. It
// lets other implementations of a workspace provider hand out leases.
func NewLease(dir string, writableRoots []string, checkouts []Checkout, release func()) *Lease {
	return &Lease{Dir: dir, WritableRoots: writableRoots, Checkouts: checkouts, release: release}
}

// ThreadAreas returns the directories that belong to this thread alone: its
// workspace directory, unless it is shared, and its checkouts. Shared
// writable roots are excluded.
func (l *Lease) ThreadAreas() []string {
	var areas []string
	if !l.Shared {
		areas = append(areas, l.Dir)
	}
	for _, checkout := range l.Checkouts {
		areas = append(areas, checkout.Path)
	}
	return areas
}

// Release returns the lease. It is safe to call more than once.
func (l *Lease) Release() {
	l.once.Do(l.release)
}

type root struct {
	path string
	// name is the root's directory name inside a thread's checkout
	// directory; empty when the root is not the top level of a git
	// repository.
	name string
	// originURL is the repository's origin remote, which checkouts push to.
	originURL string
}

// Manager creates per-thread workspace directories and git checkouts and
// removes checkouts that have not been used for a while.
type Manager struct {
	workspaceDir string
	checkoutsDir string
	roots        []root

	mu sync.Mutex
	// idLocks serialize preparing, using, and removing one thread's
	// checkouts.
	idLocks map[string]*sync.Mutex
}

// New returns a Manager. Every writable root that is the top level of a git
// repository is worked on through per-thread checkouts; other roots stay
// shared by all threads.
func New(ctx context.Context, workspaceDir, checkoutsDir string, writableRoots []string) (*Manager, error) {
	m := &Manager{
		workspaceDir: workspaceDir,
		checkoutsDir: checkoutsDir,
		idLocks:      make(map[string]*sync.Mutex),
	}
	for _, path := range writableRoots {
		r := root{path: path}
		repo, err := isRepositoryTopLevel(ctx, path)
		if err != nil {
			return nil, err
		}
		if repo {
			sum := sha256.Sum256([]byte(path))
			r.name = filepath.Base(path) + "-" + hex.EncodeToString(sum[:4])
			if url, err := gitOutput(ctx, path, "config", "--get", "remote.origin.url"); err == nil {
				r.originURL = strings.TrimSpace(url)
			}
		}
		m.roots = append(m.roots, r)
	}
	return m, nil
}

// Repositories returns the writable roots that are worked on through
// per-thread checkouts.
func (m *Manager) Repositories() []string {
	var repos []string
	for _, r := range m.roots {
		if r.name != "" {
			repos = append(repos, r.path)
		}
	}
	return repos
}

// ThreadID derives a filesystem- and branch-safe identifier for a Slack thread.
func ThreadID(channel, threadTS string) (string, error) {
	if !memory.ValidChannelID(channel) || !validTimestamp(threadTS) {
		return "", fmt.Errorf("invalid Slack thread %q/%q", channel, threadTS)
	}
	return channel + "-" + threadTS, nil
}

// ThreadDir returns the thread's workspace directory, creating it if needed.
func (m *Manager) ThreadDir(threadID string) (string, error) {
	dir, err := threadPath(m.workspaceDir, threadID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create thread workspace: %w", err)
	}
	return dir, nil
}

// Acquire prepares the thread's workspace and checkouts for a work turn.
// Existing checkouts are reused, so later work in the thread continues on the
// same branch.
func (m *Manager) Acquire(ctx context.Context, threadID string) (*Lease, error) {
	dir, err := m.ThreadDir(threadID)
	if err != nil {
		return nil, err
	}
	threadCheckouts, err := threadPath(m.checkoutsDir, threadID)
	if err != nil {
		return nil, err
	}

	idLock := m.idLock(threadID)
	idLock.Lock()
	lease, err := m.prepare(ctx, threadID, dir, threadCheckouts)
	if err != nil {
		idLock.Unlock()
		return nil, err
	}
	lease.release = func() {
		if len(lease.Checkouts) > 0 {
			// Idle time counts from the end of the work turn.
			_ = touch(filepath.Join(threadCheckouts, lastUsedFile))
		}
		idLock.Unlock()
	}
	return lease, nil
}

func (m *Manager) prepare(ctx context.Context, threadID, dir, threadCheckouts string) (*Lease, error) {
	lease := &Lease{Dir: dir}
	for _, r := range m.roots {
		if r.name == "" {
			lease.WritableRoots = append(lease.WritableRoots, r.path)
			continue
		}
		checkout := Checkout{
			Repo:   r.path,
			Path:   filepath.Join(threadCheckouts, r.name),
			Branch: branchPrefix + threadID,
		}
		if err := ensureCheckout(ctx, checkout, r.originURL); err != nil {
			return nil, err
		}
		lease.Checkouts = append(lease.Checkouts, checkout)
		lease.WritableRoots = append(lease.WritableRoots, checkout.Path, checkout.Path+gitDirSuffix)
	}
	if len(lease.Checkouts) > 0 {
		if err := touch(filepath.Join(threadCheckouts, lastUsedFile)); err != nil {
			return nil, err
		}
	}
	return lease, nil
}

// GC removes the checkouts, and with them the thread branches, of every
// thread whose last work turn ended at or before now minus idle: an idle
// thread's work is considered finished, and anything worth keeping should
// have been pushed or merged by then. Original repositories are never
// touched. It returns the thread IDs whose checkouts were removed.
func (m *Manager) GC(now time.Time, idle time.Duration) ([]string, error) {
	entries, err := os.ReadDir(m.checkoutsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list checkouts: %w", err)
	}
	var removed []string
	var errs []error
	for _, entry := range entries {
		threadID := entry.Name()
		if !entry.IsDir() || !validThreadID(threadID) {
			continue
		}
		ok, err := m.removeIfIdle(threadID, now, idle)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed = append(removed, threadID)
		}
	}
	return removed, errors.Join(errs...)
}

func (m *Manager) removeIfIdle(threadID string, now time.Time, idle time.Duration) (bool, error) {
	// A leased thread holds its lock, so it is skipped here.
	idLock := m.idLock(threadID)
	if !idLock.TryLock() {
		return false, nil
	}
	defer idLock.Unlock()

	dir := filepath.Join(m.checkoutsDir, threadID)
	lastUsed := time.Time{}
	if info, err := os.Stat(filepath.Join(dir, lastUsedFile)); err == nil {
		lastUsed = info.ModTime()
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read last use of %q: %w", threadID, err)
	}
	if lastUsed.After(now.Add(-idle)) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove checkouts of %q: %w", threadID, err)
	}
	return true, nil
}

func (m *Manager) idLock(threadID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.idLocks[threadID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.idLocks[threadID] = lock
	}
	return lock
}

func threadPath(base, threadID string) (string, error) {
	if !validThreadID(threadID) {
		return "", fmt.Errorf("invalid thread ID %q", threadID)
	}
	return filepath.Join(base, threadID), nil
}

// ensureCheckout clones the repository's current HEAD into a thread branch.
// A local clone hardlinks the repository's objects, so it is cheap and does
// not depend on the original afterwards. When originURL is set, the
// checkout's origin points there too so the branch can be pushed.
func ensureCheckout(ctx context.Context, checkout Checkout, originURL string) error {
	gitDir := checkout.Path + gitDirSuffix
	if _, err := os.Stat(filepath.Join(checkout.Path, ".git")); err == nil {
		if _, err := os.Stat(gitDir); err == nil {
			return nil
		}
	}
	// Clear anything a failed or interrupted clone left behind.
	for _, path := range []string{checkout.Path, gitDir} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("clean checkout %q: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(checkout.Path), 0o700); err != nil {
		return fmt.Errorf("create checkout directory: %w", err)
	}
	if err := git(ctx, filepath.Dir(checkout.Path), "clone", "--quiet", "--separate-git-dir="+gitDir, checkout.Repo, checkout.Path); err != nil {
		return err
	}
	if err := git(ctx, checkout.Path, "checkout", "--quiet", "-b", checkout.Branch); err != nil {
		return err
	}
	if originURL == "" {
		return nil
	}
	return git(ctx, checkout.Path, "remote", "set-url", "origin", originURL)
}

// isRepositoryTopLevel reports whether path is the top level of a git
// repository.
func isRepositoryTopLevel(ctx context.Context, path string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect %q: %w", path, err)
	}
	out, err := gitOutput(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return false, err
	}
	topLevel, err := filepath.EvalSymlinks(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("resolve git top level of %q: %w", path, err)
	}
	return topLevel == path, nil
}

func git(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return "", fmt.Errorf("git %s in %q: %s: %w", strings.Join(args, " "), dir, text, err)
		}
		return "", fmt.Errorf("git %s in %q: %w", strings.Join(args, " "), dir, err)
	}
	return stdout.String(), nil
}

// touch records the current time as the file's modification time.
func touch(path string) error {
	now := time.Now()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("record checkout use: %w", err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		return fmt.Errorf("record checkout use: %w", err)
	}
	return nil
}

func validThreadID(id string) bool {
	channel, threadTS, ok := strings.Cut(id, "-")
	return ok && memory.ValidChannelID(channel) && validTimestamp(threadTS)
}

// validTimestamp accepts Slack message timestamps (digits and at most one
// inner dot), which keeps thread IDs valid as both directory names and git
// branch names.
func validTimestamp(value string) bool {
	if value == "" || value[0] == '.' || value[len(value)-1] == '.' {
		return false
	}
	dots := 0
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char == '.':
			dots++
		default:
			return false
		}
	}
	return dots <= 1
}
