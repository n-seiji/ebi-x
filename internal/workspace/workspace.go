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
	branchPrefix = "ebi-x/"
	lastUsedFile = ".last-used"
	// Codex keeps every directory named .git read-only even when it is a
	// writable root, which would make commits impossible.
	gitDirSuffix = ".gitdir"
)

type Checkout struct {
	Repo   string
	Path   string
	Branch string
}

// Holding the thread lock keeps the garbage collector from removing a leased
// thread's checkouts.
type Lease struct {
	Dir           string
	WritableRoots []string
	Checkouts     []Checkout
	PendingRepos  []string
	Shared        bool
	Area          Area

	once    sync.Once
	release func()
}

func NewLease(dir string, writableRoots []string, checkouts []Checkout, release func()) *Lease {
	return &Lease{Dir: dir, WritableRoots: writableRoots, Checkouts: checkouts, release: release}
}

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

func (l *Lease) Release() {
	l.once.Do(l.release)
}

type root struct {
	path      string
	name      string
	originURL string
}

type Manager struct {
	workspaceDir string
	checkoutsDir string
	closedDir    string
	roots        []root

	mu      sync.Mutex
	idLocks map[string]*sync.Mutex
}

func New(ctx context.Context, workspaceDir, checkoutsDir, closedDir string, writableRoots []string) (*Manager, error) {
	m := &Manager{
		workspaceDir: workspaceDir,
		checkoutsDir: checkoutsDir,
		closedDir:    closedDir,
		idLocks:      make(map[string]*sync.Mutex),
	}
	for _, path := range writableRoots {
		r := root{path: path}
		repo, err := IsRepositoryTopLevel(ctx, path)
		if err != nil {
			return nil, err
		}
		if repo {
			sum := sha256.Sum256([]byte(path))
			r.name = filepath.Base(path) + "-" + hex.EncodeToString(sum[:4])
			if url, err := OriginURL(ctx, path); err == nil {
				r.originURL = url
			}
		}
		m.roots = append(m.roots, r)
	}
	return m, nil
}

func (m *Manager) Repositories() []string {
	var repos []string
	for _, r := range m.roots {
		if r.name != "" {
			repos = append(repos, r.path)
		}
	}
	return repos
}

func (m *Manager) OriginURLs() []string {
	var urls []string
	for _, r := range m.roots {
		if r.name != "" && r.originURL != "" {
			urls = append(urls, r.originURL)
		}
	}
	return urls
}

func ThreadID(channel, threadTS string) (string, error) {
	if !memory.ValidChannelID(channel) || !validTimestamp(threadTS) {
		return "", fmt.Errorf("invalid Slack thread %q/%q", channel, threadTS)
	}
	return channel + "-" + threadTS, nil
}

type Area string

const Open Area = ""

func Closed(channel string) Area { return Area(channel) }

func (m *Manager) dirs(area Area) (string, string, error) {
	if area == Open {
		return m.workspaceDir, m.checkoutsDir, nil
	}
	if m.closedDir == "" || !memory.ValidChannelID(string(area)) {
		return "", "", fmt.Errorf("invalid closed area %q", area)
	}
	base := filepath.Join(m.closedDir, string(area))
	return filepath.Join(base, "workspace"), filepath.Join(base, "checkouts"), nil
}

func (m *Manager) ThreadDir(threadID string, area Area) (string, error) {
	workspaceDir, _, err := m.dirs(area)
	if err != nil {
		return "", err
	}
	dir, err := threadPath(workspaceDir, threadID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create thread workspace: %w", err)
	}
	return dir, nil
}

// Missing checkouts are cloned only when createCheckouts is set, so a request
// that only reads code does not pay for clones.
func (m *Manager) Acquire(ctx context.Context, threadID string, area Area, createCheckouts bool) (*Lease, error) {
	area = m.settledArea(threadID, area)
	dir, err := m.ThreadDir(threadID, area)
	if err != nil {
		return nil, err
	}
	_, checkoutsDir, err := m.dirs(area)
	if err != nil {
		return nil, err
	}
	threadCheckouts, err := threadPath(checkoutsDir, threadID)
	if err != nil {
		return nil, err
	}

	idLock := m.idLock(threadID)
	idLock.Lock()
	lease, err := m.prepare(ctx, threadID, dir, threadCheckouts, area != Open, createCheckouts)
	if err != nil {
		idLock.Unlock()
		return nil, err
	}
	lease.Area = area
	lease.release = func() {
		if len(lease.Checkouts) > 0 {
			// Idle time counts from the end of the work turn.
			_ = touch(filepath.Join(threadCheckouts, lastUsedFile))
		}
		idLock.Unlock()
	}
	return lease, nil
}

// Not moved when the channel's kind changes: the session and checkouts point
// at these paths, and moving a checkout breaks its git directory.
func (m *Manager) settledArea(threadID string, area Area) Area {
	channel, _, _ := strings.Cut(threadID, "-")
	closedWorkspace, _, err := m.dirs(Closed(channel))
	if err != nil {
		return area
	}
	if _, err := os.Stat(filepath.Join(closedWorkspace, threadID)); err == nil {
		return Closed(channel)
	}
	if area == Open {
		return area
	}
	if _, err := os.Stat(filepath.Join(m.workspaceDir, threadID)); err == nil {
		return Open
	}
	return area
}

// Per closed channel, not per thread, so the list does not grow with threads.
func (m *Manager) Denied(area Area) ([]string, error) {
	if area == Open {
		if m.closedDir == "" {
			return nil, nil
		}
		return []string{m.closedDir}, nil
	}
	denied := []string{m.workspaceDir, m.checkoutsDir}
	for _, r := range m.roots {
		if r.name == "" {
			denied = append(denied, r.path)
		}
	}
	entries, err := os.ReadDir(m.closedDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list closed channels: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != string(area) {
			denied = append(denied, filepath.Join(m.closedDir, entry.Name()))
		}
	}
	return denied, nil
}

func (m *Manager) prepare(ctx context.Context, threadID, dir, threadCheckouts string, closed, createCheckouts bool) (*Lease, error) {
	lease := &Lease{Dir: dir}
	for _, r := range m.roots {
		if r.name == "" {
			if !closed {
				lease.WritableRoots = append(lease.WritableRoots, r.path)
			}
			continue
		}
		checkout := Checkout{
			Repo:   r.path,
			Path:   filepath.Join(threadCheckouts, r.name),
			Branch: branchPrefix + threadID,
		}
		if !createCheckouts && !checkoutReady(checkout) {
			lease.PendingRepos = append(lease.PendingRepos, r.path)
			continue
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

// An idle thread's work is considered finished: anything worth keeping should
// have been pushed or merged by then.
func (m *Manager) GC(now time.Time, idle time.Duration) ([]string, error) {
	dirs := []string{m.checkoutsDir}
	if m.closedDir != "" {
		channels, err := os.ReadDir(m.closedDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("list closed channels: %w", err)
		}
		for _, channel := range channels {
			if channel.IsDir() && memory.ValidChannelID(channel.Name()) {
				dirs = append(dirs, filepath.Join(m.closedDir, channel.Name(), "checkouts"))
			}
		}
	}
	var removed []string
	var errs []error
	for _, checkoutsDir := range dirs {
		entries, err := os.ReadDir(checkoutsDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("list checkouts: %w", err))
			continue
		}
		for _, entry := range entries {
			threadID := entry.Name()
			if !entry.IsDir() || !validThreadID(threadID) {
				continue
			}
			ok, err := m.removeIfIdle(checkoutsDir, threadID, now, idle)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				removed = append(removed, threadID)
			}
		}
	}
	return removed, errors.Join(errs...)
}

func (m *Manager) removeIfIdle(checkoutsDir, threadID string, now time.Time, idle time.Duration) (bool, error) {
	// A leased thread holds its lock, so it is skipped here.
	idLock := m.idLock(threadID)
	if !idLock.TryLock() {
		return false, nil
	}
	defer idLock.Unlock()

	dir := filepath.Join(checkoutsDir, threadID)
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

// Objects are copied, not hardlinked: the checkout's git directory is writable
// by Codex, and a shared inode would let it rewrite the original repository's
// objects in place.
func ensureCheckout(ctx context.Context, checkout Checkout, originURL string) error {
	if checkoutReady(checkout) {
		return nil
	}
	gitDir := checkout.Path + gitDirSuffix
	// Clear anything a failed or interrupted clone left behind.
	for _, path := range []string{checkout.Path, gitDir} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("clean checkout %q: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(checkout.Path), 0o700); err != nil {
		return fmt.Errorf("create checkout directory: %w", err)
	}
	if err := git(ctx, filepath.Dir(checkout.Path), "clone", "--quiet", "--no-hardlinks", "--separate-git-dir="+gitDir, checkout.Repo, checkout.Path); err != nil {
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

func checkoutReady(checkout Checkout) bool {
	if _, err := os.Stat(filepath.Join(checkout.Path, ".git")); err != nil {
		return false
	}
	_, err := os.Stat(checkout.Path + gitDirSuffix)
	return err == nil
}

// The configured remote.origin.url, before any url.*.insteadOf rewriting.
func OriginURL(ctx context.Context, path string) (string, error) {
	out, err := gitOutput(ctx, path, "config", "--get", "remote.origin.url")
	if err != nil {
		// git config --get exits 1 when the key is not set.
		if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func IsRepositoryTopLevel(ctx context.Context, path string) (bool, error) {
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

// Keeps thread IDs valid as both directory names and git branch names.
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
