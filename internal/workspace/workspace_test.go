package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func newManager(t *testing.T, roots ...string) (*Manager, string, string) {
	t.Helper()
	home := t.TempDir()
	workspaceDir := filepath.Join(home, "workspace")
	checkoutsDir := filepath.Join(home, "checkouts")
	m, err := New(context.Background(), workspaceDir, checkoutsDir, roots)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, workspaceDir, checkoutsDir
}

func TestThreadID(t *testing.T) {
	if got, err := ThreadID("C123", "1727.000100"); err != nil || got != "C123-1727.000100" {
		t.Fatalf("ThreadID() = %q, %v", got, err)
	}
	for _, tc := range [][2]string{
		{"", "1.2"}, {"C1", ""}, {"c1", "1.2"}, {"C1/..", "1.2"},
		{"C1", "../1"}, {"C1", "1..2"}, {"C1", ".1"}, {"C1", "1.2.3"}, {"C-1", "1.2"}, {"X1", "1.2"},
	} {
		if _, err := ThreadID(tc[0], tc[1]); err == nil {
			t.Errorf("ThreadID(%q, %q) succeeded, want error", tc[0], tc[1])
		}
	}
}

func TestAcquireCreatesThreadCheckoutAndKeepsPlainRootsShared(t *testing.T) {
	repo := newRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.com/app.git")
	plain := t.TempDir()
	m, workspaceDir, _ := newManager(t, plain, repo)
	if got := m.Repositories(); !reflect.DeepEqual(got, []string{repo}) {
		t.Fatalf("Repositories() = %v, want [%s]", got, repo)
	}

	lease, err := m.Acquire(context.Background(), "C1-100.1", true)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	if want := filepath.Join(workspaceDir, "C1-100.1"); lease.Dir != want {
		t.Errorf("Dir = %q, want %q", lease.Dir, want)
	}
	if len(lease.Checkouts) != 1 {
		t.Fatalf("Checkouts = %v, want one", lease.Checkouts)
	}
	checkout := lease.Checkouts[0]
	if checkout.Repo != repo || checkout.Branch != "ebi-x/C1-100.1" {
		t.Errorf("checkout = %+v", checkout)
	}
	if got := runGit(t, checkout.Path, "branch", "--show-current"); got != "ebi-x/C1-100.1" {
		t.Errorf("checkout branch = %q", got)
	}
	if got := runGit(t, checkout.Path, "remote", "get-url", "origin"); got != "https://example.com/app.git" {
		t.Errorf("checkout origin = %q, want the repository's origin", got)
	}
	// The git directory must not be named .git, which Codex keeps read-only.
	gitDir := runGit(t, checkout.Path, "rev-parse", "--absolute-git-dir")
	if gitDir != checkout.Path+gitDirSuffix {
		t.Errorf("git dir = %q, want %q", gitDir, checkout.Path+gitDirSuffix)
	}
	want := []string{plain, checkout.Path, checkout.Path + gitDirSuffix}
	if !reflect.DeepEqual(lease.WritableRoots, want) {
		t.Errorf("WritableRoots = %v, want %v", lease.WritableRoots, want)
	}
	if got := runGit(t, repo, "branch", "--list", "ebi-x/*"); got != "" {
		t.Errorf("original repository gained branches: %q", got)
	}
}

func TestThreadsGetSeparateCheckouts(t *testing.T) {
	repo := newRepo(t)
	m, _, _ := newManager(t, repo)
	first, err := m.Acquire(context.Background(), "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := m.Acquire(context.Background(), "C1-200.2", true)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.Checkouts[0].Path == second.Checkouts[0].Path || first.Dir == second.Dir {
		t.Fatal("threads share a workspace")
	}
	if err := os.WriteFile(filepath.Join(first.Checkouts[0].Path, "README.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(second.Checkouts[0].Path, "README.md"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("second checkout README = %q, %v", data, err)
	}
}

func TestAcquireReusesCheckoutWithCommits(t *testing.T) {
	repo := newRepo(t)
	m, _, _ := newManager(t, repo)
	ctx := context.Background()
	lease, err := m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Checkouts[0].Path
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", ".")
	runGit(t, path, "commit", "-q", "-m", "work")
	lease.Release()

	lease, err = m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if got := runGit(t, lease.Checkouts[0].Path, "log", "-1", "--format=%s"); got != "work" {
		t.Fatalf("reused checkout head = %q, want the earlier commit", got)
	}
}

func TestGCRemovesIdleCheckoutsAndTheirBranches(t *testing.T) {
	repo := newRepo(t)
	m, workspaceDir, checkoutsDir := newManager(t, repo)
	ctx := context.Background()

	lease, err := m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Checkouts[0].Path
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", ".")
	runGit(t, path, "commit", "-q", "-m", "work")
	lease.Release()

	now := time.Now()
	idle := 120 * time.Hour
	if removed, err := m.GC(now, idle); err != nil || len(removed) != 0 {
		t.Fatalf("GC() on fresh checkout = %v, %v; want nothing removed", removed, err)
	}

	stale := now.Add(-idle - time.Minute)
	if err := os.Chtimes(filepath.Join(checkoutsDir, "C1-100.1", lastUsedFile), stale, stale); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GC(now, idle)
	if err != nil || !reflect.DeepEqual(removed, []string{"C1-100.1"}) {
		t.Fatalf("GC() = %v, %v; want the idle thread removed", removed, err)
	}
	if _, err := os.Stat(filepath.Join(checkoutsDir, "C1-100.1")); !os.IsNotExist(err) {
		t.Fatalf("checkout directory still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "C1-100.1")); err != nil {
		t.Fatalf("thread workspace was removed: %v", err)
	}

	// Asking the thread for more work starts again from the repository HEAD.
	lease, err = m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatalf("Acquire() after GC error = %v", err)
	}
	defer lease.Release()
	if _, err := os.Stat(filepath.Join(lease.Checkouts[0].Path, "work.txt")); !os.IsNotExist(err) {
		t.Fatalf("work from the removed branch reappeared: %v", err)
	}
}

func TestGCSkipsLeasedThreads(t *testing.T) {
	repo := newRepo(t)
	m, _, checkoutsDir := newManager(t, repo)
	lease, err := m.Acquire(context.Background(), "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-200 * time.Hour)
	if err := os.Chtimes(filepath.Join(checkoutsDir, "C1-100.1", lastUsedFile), stale, stale); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.GC(time.Now(), 120*time.Hour); err != nil || len(removed) != 0 {
		t.Fatalf("GC() = %v, %v; want leased thread kept", removed, err)
	}
	lease.Release()
	if _, err := os.Stat(lease.Checkouts[0].Path); err != nil {
		t.Fatalf("leased checkout removed: %v", err)
	}
}

func TestAcquireRecreatesPartiallyRemovedCheckout(t *testing.T) {
	repo := newRepo(t)
	m, _, _ := newManager(t, repo)
	ctx := context.Background()
	lease, err := m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	path := lease.Checkouts[0].Path
	if err := os.RemoveAll(path + gitDirSuffix); err != nil {
		t.Fatal(err)
	}
	lease, err = m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	if got := runGit(t, path, "branch", "--show-current"); got != "ebi-x/C1-100.1" {
		t.Fatalf("recreated checkout branch = %q", got)
	}
}

func TestSubdirectoryOfRepositoryIsAPlainRoot(t *testing.T) {
	repo := newRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	m, _, _ := newManager(t, sub)
	if got := m.Repositories(); len(got) != 0 {
		t.Fatalf("Repositories() = %v, want none", got)
	}
	lease, err := m.Acquire(context.Background(), "C1-100.1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !reflect.DeepEqual(lease.WritableRoots, []string{sub}) || len(lease.Checkouts) != 0 {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestOtherThreadPaths(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	checkoutsDir := filepath.Join(base, "checkouts")
	for _, dir := range []string{
		filepath.Join(workspaceDir, "C1-1.1"),
		filepath.Join(workspaceDir, "C1-2.2"),
		filepath.Join(workspaceDir, "not-a-thread"),
		filepath.Join(checkoutsDir, "C1-1.1"),
		filepath.Join(checkoutsDir, "D9-3.3"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := New(context.Background(), workspaceDir, checkoutsDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := manager.OtherThreadPaths("C1-1.1")
	if err != nil {
		t.Fatalf("OtherThreadPaths() error = %v", err)
	}
	want := []string{
		filepath.Join(workspaceDir, "C1-2.2"),
		filepath.Join(workspaceDir, "not-a-thread"),
		filepath.Join(checkoutsDir, "D9-3.3"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OtherThreadPaths() = %v, want %v", got, want)
	}
}

func TestAcquireWithoutCreatingListsPendingRepositories(t *testing.T) {
	repo := newRepo(t)
	plain := t.TempDir()
	m, _, checkoutsDir := newManager(t, plain, repo)
	ctx := context.Background()

	lease, err := m.Acquire(ctx, "C1-100.1", false)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if len(lease.Checkouts) != 0 || !reflect.DeepEqual(lease.PendingRepos, []string{repo}) {
		t.Fatalf("Checkouts = %v, PendingRepos = %v, want only pending %s", lease.Checkouts, lease.PendingRepos, repo)
	}
	if !reflect.DeepEqual(lease.WritableRoots, []string{plain}) {
		t.Errorf("WritableRoots = %v, want [%s]", lease.WritableRoots, plain)
	}
	lease.Release()
	if _, err := os.Stat(filepath.Join(checkoutsDir, "C1-100.1")); !os.IsNotExist(err) {
		t.Fatalf("thread checkouts directory exists without a request: %v", err)
	}

	lease, err = m.Acquire(ctx, "C1-100.1", true)
	if err != nil {
		t.Fatalf("Acquire() creating error = %v", err)
	}
	if len(lease.Checkouts) != 1 || len(lease.PendingRepos) != 0 {
		t.Fatalf("Checkouts = %v, PendingRepos = %v, want one checkout", lease.Checkouts, lease.PendingRepos)
	}
	lease.Release()

	// Once cloned, later turns reuse the checkout without asking again.
	lease, err = m.Acquire(ctx, "C1-100.1", false)
	if err != nil {
		t.Fatalf("Acquire() reuse error = %v", err)
	}
	defer lease.Release()
	if len(lease.Checkouts) != 1 || len(lease.PendingRepos) != 0 {
		t.Fatalf("Checkouts = %v, PendingRepos = %v, want the existing checkout", lease.Checkouts, lease.PendingRepos)
	}
}
