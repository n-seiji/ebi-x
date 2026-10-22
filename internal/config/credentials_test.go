package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// credentialTestHome points HOME at a temp directory and isolates git from
// the machine's own configuration. It returns the symlink-resolved home.
func credentialTestHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig-test"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

// writeGlobalGitConfig sets the global git configuration, which is what a
// thread checkout sees besides the origin URL.
func writeGlobalGitConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// resolve runs resolveCredentialPaths with a fresh checkouts directory.
func resolve(t *testing.T, roots, protected []string, allowed string) ([]string, error) {
	t.Helper()
	return resolveCredentialPaths(roots, protected, allowed, filepath.Join(t.TempDir(), "checkouts"))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v error = %v: %s", args, err, out)
	}
}

// newCredentialRepo creates a git repository with the given origin (none if
// empty).
func newCredentialRepo(t *testing.T, origin string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	if origin != "" {
		runGit(t, root, "remote", "add", "origin", origin)
	}
	return root
}

func TestResolveCredentialPathsDefaults(t *testing.T) {
	home := credentialTestHome(t)

	got, err := resolve(t, nil, nil, "")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	for _, rel := range []string{
		".ssh", ".aws", ".azure", ".config/gcloud", ".config/gh", ".docker/config.json", ".kube",
		".gnupg", ".netrc", ".git-credentials", ".config/git/credentials", ".npmrc", ".pypirc",
		".config/op", ".vault-token", ".terraform.d/credentials.tfrc.json", ".cargo/credentials.toml",
		".gem/credentials", ".pgpass", ".my.cnf", ".s3cfg", ".boto", ".oci", ".config/rclone",
		".claude", ".config/github-copilot",
	} {
		if want := filepath.Join(home, rel); !slices.Contains(got, want) {
			t.Errorf("resolveCredentialPaths() = %v; want it to contain %q", got, want)
		}
	}
	if len(got) != len(credentialPaths) {
		t.Errorf("resolveCredentialPaths() has %d paths; want %d", len(got), len(credentialPaths))
	}
	for _, rel := range []string{".docker", ".m2", ".gradle"} {
		if path := filepath.Join(home, rel); slices.Contains(got, path) {
			t.Errorf("resolveCredentialPaths() = %v; want %q left readable", got, path)
		}
	}
}

// assertExempt checks that resolving the credential paths for the given
// roots denies every default path except the home-relative ones in exempt.
func assertExempt(t *testing.T, home string, roots []string, exempt []string) {
	t.Helper()
	got, err := resolve(t, roots, nil, "")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	want, err := resolve(t, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, rel := range exempt {
		paths = append(paths, filepath.Join(home, rel))
	}
	want = slices.DeleteFunc(want, func(p string) bool { return slices.Contains(paths, p) })
	if !slices.Equal(got, want) {
		t.Errorf("resolveCredentialPaths() = %v; want %v", got, want)
	}
}

func TestResolveCredentialPathsPushExemption(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		config string   // global git config
		want   []string // home-relative paths that must not be denied
	}{
		{"https with no helper", "https://github.com/o/r.git", "", []string{".netrc"}},
		{"https origin with credentials and no helper", "https://user:secret@github.com/o/r.git", "", []string{".netrc"}},
		{"ssh scp-like origin", "git@github.com:o/r.git", "", []string{".ssh"}},
		{"ssh scheme origin", "ssh://git@github.com/o/r.git", "", []string{".ssh"}},
		{"store helper", "https://github.com/o/r.git", "[credential]\n\thelper = store\n", []string{".git-credentials", ".config/git/credentials"}},
		{"store helper with file", "https://github.com/o/r.git", "[credential]\n\thelper = store --file ~/.config/custom-creds\n", []string{".config/custom-creds"}},
		{"store helper with file=", "https://github.com/o/r.git", "[credential]\n\thelper = store --file=~/.git-credentials\n", []string{".git-credentials"}},
		{"gh helper", "https://github.com/o/r.git", "[credential]\n\thelper = !gh auth git-credential\n", []string{".config/gh"}},
		{"gh helper with absolute path", "https://github.com/o/r.git", "[credential]\n\thelper = !/opt/homebrew/bin/gh auth git-credential\n", []string{".config/gh"}},
		{"url-specific gh helper", "https://github.com/o/r.git", "[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n", []string{".config/gh"}},
		{"chain of keychain then gh", "https://github.com/o/r.git", "[credential]\n\thelper = osxkeychain\n\thelper = !gh auth git-credential\n", []string{".config/gh"}},
		{"chain of store then keychain", "https://github.com/o/r.git", "[credential]\n\thelper = store\n\thelper = osxkeychain\n", []string{".git-credentials", ".config/git/credentials"}},
		{"empty value reset is ignored", "https://github.com/o/r.git", "[credential]\n\thelper = store\n\thelper =\n\thelper = osxkeychain\n", []string{".git-credentials", ".config/git/credentials"}},
		{"helpers from every credential section", "https://github.com/o/r.git", "[credential]\n\thelper = osxkeychain\n[credential \"https://github.com\"]\n\thelper = store\n[credential \"https://github.com/o\"]\n\thelper = !gh auth git-credential\n", []string{".git-credentials", ".config/git/credentials", ".config/gh"}},
		{"helper scoped to another host", "https://github.com/o/r.git", "[credential \"https://example.com\"]\n\thelper = store\n", []string{".git-credentials", ".config/git/credentials"}},
		{"osxkeychain helper", "https://github.com/o/r.git", "[credential]\n\thelper = osxkeychain\n", nil},
		{"cache helper", "https://github.com/o/r.git", "[credential]\n\thelper = cache --timeout 60\n", nil},
		{"pushInsteadOf rewrites https to ssh", "https://github.com/o/r.git", "[url \"git@github.com:\"]\n\tpushInsteadOf = https://github.com/\n", []string{".ssh"}},
		{"insteadOf rewrites https to ssh", "https://github.com/o/r.git", "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n", []string{".ssh"}},
		{"no origin", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := credentialTestHome(t)
			writeGlobalGitConfig(t, tt.config)
			root := newCredentialRepo(t, tt.origin)
			assertExempt(t, home, []string{root}, tt.want)
		})
	}
}

func TestResolveCredentialPathsIgnoresRepositoryLocalConfig(t *testing.T) {
	home := credentialTestHome(t)
	root := newCredentialRepo(t, "https://github.com/o/r.git")
	// A thread checkout is a fresh clone: it does not inherit these.
	runGit(t, root, "config", "credential.helper", "store")
	runGit(t, root, "config", "url.git@github.com:.pushInsteadOf", "https://github.com/")
	runGit(t, root, "config", "commit.gpgsign", "true")

	assertExempt(t, home, []string{root}, []string{".netrc"})
}

func TestResolveCredentialPathsCommitSigning(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{"openpgp by default", "[commit]\n\tgpgsign = true\n", []string{".gnupg"}},
		{"explicit openpgp", "[commit]\n\tgpgsign = true\n[gpg]\n\tformat = openpgp\n", []string{".gnupg"}},
		{"ssh", "[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n", []string{".ssh"}},
		{"x509", "[commit]\n\tgpgsign = true\n[gpg]\n\tformat = x509\n", nil},
		{"signing disabled", "[commit]\n\tgpgsign = false\n[gpg]\n\tformat = ssh\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := credentialTestHome(t)
			writeGlobalGitConfig(t, tt.config)
			// No origin: signing alone exempts.
			root := newCredentialRepo(t, "")
			assertExempt(t, home, []string{root}, tt.want)
		})
	}
}

func TestResolveCredentialPathsRemovesProbe(t *testing.T) {
	credentialTestHome(t)
	root := newCredentialRepo(t, "git@github.com:o/r.git")
	checkouts := filepath.Join(t.TempDir(), "checkouts")

	if _, err := resolveCredentialPaths([]string{root}, nil, "", checkouts); err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	entries, err := os.ReadDir(checkouts)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("checkouts directory has %d leftover entries; want 0", len(entries))
	}
}

func TestResolveCredentialPathsRejectsUnknownAllowedEntry(t *testing.T) {
	credentialTestHome(t)

	_, err := resolve(t, nil, nil, "~/.kube,~/.ssh-extra")
	if err == nil {
		t.Fatal("resolveCredentialPaths() error = nil, want error")
	}
	for _, want := range []string{"~/.ssh-extra", "~/.kube", "~/.aws"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resolveCredentialPaths() error = %q; want it to mention %q", err, want)
		}
	}
}

func TestResolveCredentialPathsIgnoresPlainDirectories(t *testing.T) {
	credentialTestHome(t)
	want, err := resolve(t, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolve(t, []string{t.TempDir()}, nil, "")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("resolveCredentialPaths() = %v; want %v", got, want)
	}
}

func TestResolveCredentialPathsAllowedReadPaths(t *testing.T) {
	home := credentialTestHome(t)

	got, err := resolve(t, nil, nil, "~/.config/gcloud, ~/.kube")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	for _, rel := range []string{".config/gcloud", ".kube"} {
		if path := filepath.Join(home, rel); slices.Contains(got, path) {
			t.Errorf("resolveCredentialPaths() = %v; want %q removed", got, path)
		}
	}
	if len(got) != len(credentialPaths)-2 {
		t.Errorf("resolveCredentialPaths() has %d paths; want %d", len(got), len(credentialPaths)-2)
	}
}

func TestResolveCredentialPathsKeepsProtectedPaths(t *testing.T) {
	home := credentialTestHome(t)
	ssh := filepath.Join(home, ".ssh")
	root := newCredentialRepo(t, "git@github.com:o/r.git")

	// A protected path is denied through ProtectedPaths, so the credential
	// list neither repeats nor un-denies it.
	got, err := resolve(t, []string{root}, []string{ssh}, "~/.ssh")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	if slices.Contains(got, ssh) {
		t.Errorf("resolveCredentialPaths() = %v; want %q left to ProtectedPaths", got, ssh)
	}
}

func TestLoadAllowedReadPathsCannotUndenyProtectedPaths(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	setRequiredEnv(t)
	home := credentialTestHome(t)
	t.Setenv("EBIX_HOME", filepath.Join(workingDir, "ebix"))
	t.Setenv("EBIX_DENIED_READ_PATHS", "~/.aws")
	t.Setenv("EBIX_ALLOWED_READ_PATHS", "~/.aws,~/.config/gcloud")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if want := filepath.Join(home, ".aws"); !slices.Contains(cfg.ProtectedPaths, want) {
		t.Errorf("ProtectedPaths = %v; want it to contain %q", cfg.ProtectedPaths, want)
	}
	if want := filepath.Join(workingDir, ".env"); !slices.ContainsFunc(cfg.ProtectedPaths, func(p string) bool { return filepath.Base(p) == ".env" }) {
		t.Errorf("ProtectedPaths = %v; want it to contain %q", cfg.ProtectedPaths, want)
	}
	if want := filepath.Join(home, ".config", "gcloud"); slices.Contains(cfg.CredentialPaths, want) {
		t.Errorf("CredentialPaths = %v; want %q removed", cfg.CredentialPaths, want)
	}
	if want := filepath.Join(home, ".ssh"); !slices.Contains(cfg.CredentialPaths, want) {
		t.Errorf("CredentialPaths = %v; want it to contain %q", cfg.CredentialPaths, want)
	}
}

func TestLoadAcceptsWritableRootContainingHome(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	setRequiredEnv(t)
	home := credentialTestHome(t)
	t.Setenv("EBIX_HOME", filepath.Join(workingDir, "ebix"))
	t.Setenv("EBIX_WRITABLE_ROOTS", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if want := filepath.Join(home, ".ssh"); !slices.Contains(cfg.CredentialPaths, want) {
		t.Errorf("CredentialPaths = %v; want it to contain %q", cfg.CredentialPaths, want)
	}
}

func TestLoadRejectsAllowedReadPathsOutsideDefaults(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	setRequiredEnv(t)
	credentialTestHome(t)
	t.Setenv("EBIX_HOME", filepath.Join(workingDir, "ebix"))
	t.Setenv("EBIX_ALLOWED_READ_PATHS", ".env")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `".env"`) {
		t.Errorf("Load() error = %v; want it to name the .env entry", err)
	}
}

func TestResolveCredentialPathsRemovesStaleProbes(t *testing.T) {
	credentialTestHome(t)
	root := newCredentialRepo(t, "git@github.com:o/r.git")
	checkouts := filepath.Join(t.TempDir(), "checkouts")
	stale := filepath.Join(checkouts, ".credprobe-leftover")
	keep := filepath.Join(checkouts, "thread-checkout")
	for _, dir := range []string{stale, keep} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := resolveCredentialPaths([]string{root}, nil, "", checkouts); err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale probe %s still exists (stat error = %v); want removed", stale, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated checkout %s was removed: %v", keep, err)
	}
}
