package config

import (
	"os/exec"
	"path/filepath"
	"slices"
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
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "nonexistent-gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v error = %v: %s", args, err, out)
	}
}

// newCredentialRepo creates a git repository with the given origin (none if
// empty) and credential helper (none if empty).
func newCredentialRepo(t *testing.T, origin, helper string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	if origin != "" {
		runGit(t, root, "remote", "add", "origin", origin)
	}
	if helper != "" {
		runGit(t, root, "config", "credential.helper", helper)
	}
	return root
}

func TestResolveCredentialPathsDefaults(t *testing.T) {
	home := credentialTestHome(t)

	got, err := resolveCredentialPaths(nil, nil, "")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	for _, rel := range []string{
		".ssh", ".aws", ".azure", ".config/gcloud", ".config/gh", ".docker", ".kube",
		".gnupg", ".netrc", ".git-credentials", ".config/git/credentials", ".npmrc", ".pypirc",
	} {
		if want := filepath.Join(home, rel); !slices.Contains(got, want) {
			t.Errorf("resolveCredentialPaths() = %v; want it to contain %q", got, want)
		}
	}
	if len(got) != len(credentialPaths) {
		t.Errorf("resolveCredentialPaths() has %d paths; want %d", len(got), len(credentialPaths))
	}
}

func TestResolveCredentialPathsPushExemption(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		helper string
		want   []string // home-relative paths that must not be denied
	}{
		{"https with no helper", "https://github.com/o/r.git", "", []string{".netrc"}},
		{"https origin with credentials and no helper", "https://user:secret@github.com/o/r.git", "", []string{".netrc"}},
		{"ssh scp-like origin", "git@github.com:o/r.git", "", []string{".ssh"}},
		{"ssh scheme origin", "ssh://git@github.com/o/r.git", "", []string{".ssh"}},
		{"store helper", "https://github.com/o/r.git", "store", []string{".git-credentials", ".config/git/credentials"}},
		{"store helper with file", "https://github.com/o/r.git", "store --file ~/.config/custom-creds", []string{".config/custom-creds"}},
		{"store helper with file=", "https://github.com/o/r.git", "store --file=~/.git-credentials", []string{".git-credentials"}},
		{"gh helper", "https://github.com/o/r.git", "!gh auth git-credential", []string{".config/gh"}},
		{"gh helper with absolute path", "https://github.com/o/r.git", "!/opt/homebrew/bin/gh auth git-credential", []string{".config/gh"}},
		{"osxkeychain helper", "https://github.com/o/r.git", "osxkeychain", nil},
		{"cache helper", "https://github.com/o/r.git", "cache --timeout 60", nil},
		{"no origin", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := credentialTestHome(t)
			root := newCredentialRepo(t, tt.origin, tt.helper)

			got, err := resolveCredentialPaths([]string{root}, nil, "")
			if err != nil {
				t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
			}
			want, err := resolveCredentialPaths(nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			var exempt []string
			for _, rel := range tt.want {
				exempt = append(exempt, filepath.Join(home, rel))
			}
			want = slices.DeleteFunc(want, func(p string) bool { return slices.Contains(exempt, p) })
			if !slices.Equal(got, want) {
				t.Errorf("resolveCredentialPaths() = %v; want %v", got, want)
			}
		})
	}
}

func TestResolveCredentialPathsIgnoresPlainDirectories(t *testing.T) {
	credentialTestHome(t)
	want, err := resolveCredentialPaths(nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveCredentialPaths([]string{t.TempDir()}, nil, "")
	if err != nil {
		t.Fatalf("resolveCredentialPaths() error = %v, want nil", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("resolveCredentialPaths() = %v; want %v", got, want)
	}
}

func TestResolveCredentialPathsAllowedReadPaths(t *testing.T) {
	home := credentialTestHome(t)

	got, err := resolveCredentialPaths(nil, nil, "~/.config/gcloud, ~/.kube")
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
	root := newCredentialRepo(t, "git@github.com:o/r.git", "")

	// A protected path is denied through ProtectedPaths, so the credential
	// list neither repeats nor un-denies it.
	got, err := resolveCredentialPaths([]string{root}, []string{ssh}, "~/.ssh")
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
	t.Setenv("EBIX_ALLOWED_READ_PATHS", "~/.aws,~/.config/gcloud,.env")

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
