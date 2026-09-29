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
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
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
	worktreesDir := filepath.Join(home, "worktrees")
	m, err := New(context.Background(), workspaceDir, worktreesDir, roots)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, workspaceDir, worktreesDir
}

func TestThreadID(t *testing.T) {
	if got, err := ThreadID("C123", "1727.000100"); err != nil || got != "C123-1727.000100" {
		t.Fatalf("ThreadID() = %q, %v", got, err)
	}
	for _, tc := range [][2]string{
		{"", "1.2"}, {"C1", ""}, {"c1", "1.2"}, {"C1/..", "1.2"},
		{"C1", "../1"}, {"C1", "1..2"}, {"C1", ".1"}, {"C1", "1.2.3"}, {"C-1", "1.2"},
	} {
		if _, err := ThreadID(tc[0], tc[1]); err == nil {
			t.Errorf("ThreadID(%q, %q) succeeded, want error", tc[0], tc[1])
		}
	}
}

func TestAcquireCreatesThreadWorktreeAndKeepsPlainRootsShared(t *testing.T) {
	repo := newRepo(t)
	plain := t.TempDir()
	m, workspaceDir, _ := newManager(t, plain, repo)
	if got := m.Repositories(); !reflect.DeepEqual(got, []string{repo}) {
		t.Fatalf("Repositories() = %v, want [%s]", got, repo)
	}

	lease, err := m.Acquire(context.Background(), "C1-100.1")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	if want := filepath.Join(workspaceDir, "C1-100.1"); lease.Dir != want {
		t.Errorf("Dir = %q, want %q", lease.Dir, want)
	}
	if len(lease.Worktrees) != 1 {
		t.Fatalf("Worktrees = %v, want one", lease.Worktrees)
	}
	worktree := lease.Worktrees[0]
	if worktree.Repo != repo || worktree.Branch != "ebi-x/C1-100.1" {
		t.Errorf("worktree = %+v", worktree)
	}
	if got := runGit(t, worktree.Path, "branch", "--show-current"); got != "ebi-x/C1-100.1" {
		t.Errorf("worktree branch = %q", got)
	}
	gitDir := filepath.Join(repo, ".git")
	want := []string{plain, worktree.Path, gitDir}
	if !reflect.DeepEqual(lease.WritableRoots, want) {
		t.Errorf("WritableRoots = %v, want %v", lease.WritableRoots, want)
	}
	if got := runGit(t, repo, "branch", "--show-current"); got != "main" {
		t.Errorf("original checkout moved to %q", got)
	}
}

func TestThreadsGetSeparateWorktrees(t *testing.T) {
	repo := newRepo(t)
	m, _, _ := newManager(t, repo)
	first, err := m.Acquire(context.Background(), "C1-100.1")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := m.Acquire(context.Background(), "C1-200.2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.Worktrees[0].Path == second.Worktrees[0].Path || first.Dir == second.Dir {
		t.Fatal("threads share a workspace")
	}
	if err := os.WriteFile(filepath.Join(first.Worktrees[0].Path, "README.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(second.Worktrees[0].Path, "README.md"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("second worktree README = %q, %v", data, err)
	}
}

func TestGCRemovesIdleWorktreesAndBranches(t *testing.T) {
	repo := newRepo(t)
	m, workspaceDir, worktreesDir := newManager(t, repo)
	ctx := context.Background()

	lease, err := m.Acquire(ctx, "C1-100.1")
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Worktrees[0].Path
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", ".")
	runGit(t, path, "commit", "-q", "-m", "work")
	lease.Release()

	now := time.Now()
	idle := 120 * time.Hour
	if removed, err := m.GC(ctx, now, idle); err != nil || len(removed) != 0 {
		t.Fatalf("GC() on fresh worktree = %v, %v; want nothing removed", removed, err)
	}

	stale := now.Add(-idle - time.Minute)
	if err := os.Chtimes(filepath.Join(worktreesDir, "C1-100.1", lastUsedFile), stale, stale); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GC(ctx, now, idle)
	if err != nil || !reflect.DeepEqual(removed, []string{"C1-100.1"}) {
		t.Fatalf("GC() = %v, %v; want the idle thread removed", removed, err)
	}
	if _, err := os.Stat(filepath.Join(worktreesDir, "C1-100.1")); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if out := runGit(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "C1-100.1") {
		t.Fatalf("worktree still registered:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "C1-100.1")); err != nil {
		t.Fatalf("thread workspace was removed: %v", err)
	}

	if out := runGit(t, repo, "branch", "--list", "ebi-x/*"); out != "" {
		t.Fatalf("thread branch still exists: %q", out)
	}

	// Asking the thread for more work starts again from the repository HEAD.
	lease, err = m.Acquire(ctx, "C1-100.1")
	if err != nil {
		t.Fatalf("Acquire() after GC error = %v", err)
	}
	defer lease.Release()
	if _, err := os.Stat(filepath.Join(lease.Worktrees[0].Path, "work.txt")); !os.IsNotExist(err) {
		t.Fatalf("work from the removed branch reappeared: %v", err)
	}
}

func TestGCSkipsLeasedThreads(t *testing.T) {
	repo := newRepo(t)
	m, _, worktreesDir := newManager(t, repo)
	ctx := context.Background()
	lease, err := m.Acquire(ctx, "C1-100.1")
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-200 * time.Hour)
	if err := os.Chtimes(filepath.Join(worktreesDir, "C1-100.1", lastUsedFile), stale, stale); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.GC(ctx, time.Now(), 120*time.Hour); err != nil || len(removed) != 0 {
		t.Fatalf("GC() = %v, %v; want leased thread kept", removed, err)
	}
	lease.Release()
	if _, err := os.Stat(lease.Worktrees[0].Path); err != nil {
		t.Fatalf("leased worktree removed: %v", err)
	}
}

func TestAcquireRecreatesPartiallyRemovedWorktree(t *testing.T) {
	repo := newRepo(t)
	m, _, _ := newManager(t, repo)
	ctx := context.Background()
	lease, err := m.Acquire(ctx, "C1-100.1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	path := lease.Worktrees[0].Path
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err = m.Acquire(ctx, "C1-100.1")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("worktree was not recreated: %v", err)
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
	lease, err := m.Acquire(context.Background(), "C1-100.1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !reflect.DeepEqual(lease.WritableRoots, []string{sub}) || len(lease.Worktrees) != 0 {
		t.Fatalf("lease = %+v", lease)
	}
}
