// Package workspace prepares per-thread working directories and git
// worktrees so work turns for different Slack threads can run in parallel
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
)

const (
	// BranchPrefix names the branches created for thread worktrees.
	BranchPrefix = "ebi-x/"
	// lastUsedFile records, by its modification time, when a thread's
	// worktrees were last used by a work turn.
	lastUsedFile = ".last-used"
)

// Worktree is a git worktree dedicated to one Slack thread.
type Worktree struct {
	// Repo is the original repository configured as a writable root.
	Repo string
	// Path is the thread's worktree of Repo.
	Path   string
	Branch string
}

// Lease holds a thread's workspace for the duration of one work turn. The
// garbage collector never removes a leased thread's worktrees.
type Lease struct {
	// Dir is the thread's workspace directory, used as the turn's cwd.
	Dir string
	// WritableRoots replaces each git repository among the configured
	// writable roots with its thread worktree and the repository's git
	// directory, which commits in the worktree write to.
	WritableRoots []string
	Worktrees     []Worktree

	once    sync.Once
	release func()
}

// NewLease returns a lease that calls release once when it is released. It
// lets other implementations of a workspace provider hand out leases.
func NewLease(dir string, writableRoots []string, worktrees []Worktree, release func()) *Lease {
	return &Lease{Dir: dir, WritableRoots: writableRoots, Worktrees: worktrees, release: release}
}

// Release returns the lease. It is safe to call more than once.
func (l *Lease) Release() {
	l.once.Do(l.release)
}

type root struct {
	path string
	// gitDir is the repository's common git directory; empty when the root
	// is not the top level of a git repository.
	gitDir string
	// name is the root's directory name inside a thread's worktree directory.
	name string
}

// Manager creates per-thread workspace directories and git worktrees and
// removes worktrees that have not been used for a while.
type Manager struct {
	workspaceDir string
	worktreesDir string
	roots        []root

	mu      sync.Mutex
	active  map[string]int
	idLocks map[string]*sync.Mutex
}

// New returns a Manager. Every writable root that is the top level of a git
// repository is worked on through per-thread worktrees; other roots stay
// shared by all threads.
func New(ctx context.Context, workspaceDir, worktreesDir string, writableRoots []string) (*Manager, error) {
	m := &Manager{
		workspaceDir: workspaceDir,
		worktreesDir: worktreesDir,
		active:       make(map[string]int),
		idLocks:      make(map[string]*sync.Mutex),
	}
	for _, path := range writableRoots {
		r := root{path: path}
		gitDir, err := repositoryGitDir(ctx, path)
		if err != nil {
			return nil, err
		}
		if gitDir != "" {
			r.gitDir = gitDir
			sum := sha256.Sum256([]byte(path))
			r.name = filepath.Base(path) + "-" + hex.EncodeToString(sum[:4])
		}
		m.roots = append(m.roots, r)
	}
	return m, nil
}

// Repositories returns the writable roots that are worked on through
// worktrees.
func (m *Manager) Repositories() []string {
	var repos []string
	for _, r := range m.roots {
		if r.gitDir != "" {
			repos = append(repos, r.path)
		}
	}
	return repos
}

// ThreadID derives a filesystem- and branch-safe identifier for a Slack thread.
func ThreadID(channel, threadTS string) (string, error) {
	if !safeComponent(channel, false) || !safeComponent(threadTS, true) {
		return "", fmt.Errorf("invalid Slack thread %q/%q", channel, threadTS)
	}
	return channel + "-" + threadTS, nil
}

// ThreadDir returns the thread's workspace directory, creating it if needed.
func (m *Manager) ThreadDir(threadID string) (string, error) {
	dir, err := m.threadPath(m.workspaceDir, threadID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create thread workspace: %w", err)
	}
	return dir, nil
}

// Acquire prepares the thread's workspace and worktrees for a work turn.
// Existing worktrees are reused, and a branch left behind by a removed
// worktree is checked out again so earlier commits are kept.
func (m *Manager) Acquire(ctx context.Context, threadID string) (*Lease, error) {
	dir, err := m.ThreadDir(threadID)
	if err != nil {
		return nil, err
	}
	threadWorktrees, err := m.threadPath(m.worktreesDir, threadID)
	if err != nil {
		return nil, err
	}

	idLock := m.idLock(threadID)
	idLock.Lock()
	defer idLock.Unlock()

	lease := &Lease{Dir: dir}
	seenGitDirs := make(map[string]struct{})
	for _, r := range m.roots {
		if r.gitDir == "" {
			lease.WritableRoots = append(lease.WritableRoots, r.path)
			continue
		}
		worktree := Worktree{
			Repo:   r.path,
			Path:   filepath.Join(threadWorktrees, r.name),
			Branch: BranchPrefix + threadID,
		}
		if err := ensureWorktree(ctx, worktree); err != nil {
			return nil, err
		}
		lease.Worktrees = append(lease.Worktrees, worktree)
		lease.WritableRoots = append(lease.WritableRoots, worktree.Path)
		if _, seen := seenGitDirs[r.gitDir]; !seen {
			seenGitDirs[r.gitDir] = struct{}{}
			lease.WritableRoots = append(lease.WritableRoots, r.gitDir)
		}
	}
	if len(lease.Worktrees) > 0 {
		if err := touch(filepath.Join(threadWorktrees, lastUsedFile), time.Now()); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	m.active[threadID]++
	m.mu.Unlock()
	lease.release = func() {
		if len(lease.Worktrees) > 0 {
			// Idle time counts from the end of the work turn.
			_ = touch(filepath.Join(threadWorktrees, lastUsedFile), time.Now())
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active[threadID]--; m.active[threadID] <= 0 {
			delete(m.active, threadID)
		}
	}
	return lease, nil
}

// GC removes the worktrees of every thread whose last work turn ended at or
// before now minus idle. Branches are kept, so committed work survives and
// is checked out again if the thread asks for more work. It returns the
// thread IDs whose worktrees were removed.
func (m *Manager) GC(ctx context.Context, now time.Time, idle time.Duration) ([]string, error) {
	entries, err := os.ReadDir(m.worktreesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	var removed []string
	var errs []error
	for _, entry := range entries {
		threadID := entry.Name()
		if !entry.IsDir() || !validThreadID(threadID) {
			continue
		}
		ok, err := m.removeIfIdle(ctx, threadID, now, idle)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed = append(removed, threadID)
		}
	}
	for _, r := range m.roots {
		if r.gitDir == "" || len(removed) == 0 {
			continue
		}
		if err := git(ctx, r.path, "worktree", "prune"); err != nil {
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

func (m *Manager) removeIfIdle(ctx context.Context, threadID string, now time.Time, idle time.Duration) (bool, error) {
	idLock := m.idLock(threadID)
	if !idLock.TryLock() {
		return false, nil
	}
	defer idLock.Unlock()
	m.mu.Lock()
	inUse := m.active[threadID] > 0
	m.mu.Unlock()
	if inUse {
		return false, nil
	}

	dir := filepath.Join(m.worktreesDir, threadID)
	lastUsed := time.Time{}
	if info, err := os.Stat(filepath.Join(dir, lastUsedFile)); err == nil {
		lastUsed = info.ModTime()
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read last use of %q: %w", threadID, err)
	}
	if lastUsed.After(now.Add(-idle)) {
		return false, nil
	}

	var errs []error
	for _, r := range m.roots {
		if r.gitDir == "" {
			continue
		}
		path := filepath.Join(dir, r.name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := git(ctx, r.path, "worktree", "remove", "--force", path); err != nil {
			errs = append(errs, err)
		}
	}
	// Anything git did not remove, including worktrees of roots that are no
	// longer configured, is deleted; the next prune drops their registration.
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, fmt.Errorf("remove worktrees of %q: %w", threadID, err))
		return false, errors.Join(errs...)
	}
	return true, errors.Join(errs...)
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

func (m *Manager) threadPath(base, threadID string) (string, error) {
	if !validThreadID(threadID) {
		return "", fmt.Errorf("invalid thread ID %q", threadID)
	}
	return filepath.Join(base, threadID), nil
}

func ensureWorktree(ctx context.Context, worktree Worktree) error {
	if _, err := os.Stat(filepath.Join(worktree.Path, ".git")); err == nil {
		return nil
	}
	// A partially created directory or a stale registration would make
	// "worktree add" fail.
	if err := os.RemoveAll(worktree.Path); err != nil {
		return fmt.Errorf("clean worktree %q: %w", worktree.Path, err)
	}
	if err := git(ctx, worktree.Repo, "worktree", "prune"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree.Path), 0o700); err != nil {
		return fmt.Errorf("create worktree directory: %w", err)
	}
	if git(ctx, worktree.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+worktree.Branch) == nil {
		return git(ctx, worktree.Repo, "worktree", "add", worktree.Path, worktree.Branch)
	}
	return git(ctx, worktree.Repo, "worktree", "add", "-b", worktree.Branch, worktree.Path, "HEAD")
}

// repositoryGitDir returns the common git directory when path is the top
// level of a git repository, and "" otherwise.
func repositoryGitDir(ctx context.Context, path string) (string, error) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect %q: %w", path, err)
	}
	out, err := gitOutput(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "", fmt.Errorf("inspect git repository %q: unexpected output %q", path, out)
	}
	topLevel, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		return "", fmt.Errorf("resolve git top level of %q: %w", path, err)
	}
	if topLevel != path {
		return "", nil
	}
	gitDir, err := filepath.EvalSymlinks(lines[1])
	if err != nil {
		return "", fmt.Errorf("resolve git directory of %q: %w", path, err)
	}
	return gitDir, nil
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

func touch(path string, at time.Time) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("record worktree use: %w", err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		return fmt.Errorf("record worktree use: %w", err)
	}
	return nil
}

func validThreadID(id string) bool {
	channel, threadTS, ok := strings.Cut(id, "-")
	return ok && safeComponent(channel, false) && safeComponent(threadTS, true)
}

// safeComponent accepts Slack channel IDs (upper-case letters and digits)
// and message timestamps (digits and one dot), which keeps thread IDs valid
// as both directory names and git branch names.
func safeComponent(value string, timestamp bool) bool {
	if value == "" || value[0] == '.' || value[len(value)-1] == '.' {
		return false
	}
	dots := 0
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case !timestamp && char >= 'A' && char <= 'Z':
		case timestamp && char == '.':
			dots++
		default:
			return false
		}
	}
	return dots <= 1
}
