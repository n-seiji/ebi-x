package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnCredentialsReplaceHostGitHelper(t *testing.T) {
	dir := t.TempDir()
	env, err := writeTurnCredentials(dir, "github.com", "ghs_turn")
	if err != nil {
		t.Fatalf("writeTurnCredentials() error = %v", err)
	}
	// The host's helper, standing in for one that needs the App private key.
	global := filepath.Join(t.TempDir(), "gitconfig")
	hostConfig := "[credential]\n\thelper = \"!f() { echo username=host; echo password=host-secret; }; f\"\n"
	if err := os.WriteFile(global, []byte(hostConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader("url=https://github.com/miive-project/miive-roadmap\n\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git credential fill: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "password=ghs_turn\n") || strings.Contains(string(output), "host-secret") {
		t.Fatalf("git credential fill = %q, want only the turn token", output)
	}

	if gh, err := exec.LookPath("gh"); err == nil {
		// gh must read the token without migrating its configuration,
		// which the read-only sandbox would refuse.
		if err := os.Chmod(filepath.Join(dir, "gh"), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "gh"), 0o700) })
		cmd := exec.Command(gh, "auth", "token")
		cmd.Env = append(withoutGitHubTokens(os.Environ()), env...)
		output, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != "ghs_turn" {
			t.Fatalf("gh auth token = %q, %v; want the turn token", output, err)
		}
	}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		for _, marker := range []string{"KEY", "SECRET", "TOKEN"} {
			if strings.Contains(name, marker) {
				t.Fatalf("variable %s would be removed from Codex's commands", name)
			}
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

func withoutGitHubTokens(env []string) []string {
	var kept []string
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GH_TOKEN=") && !strings.HasPrefix(entry, "GITHUB_TOKEN=") && !strings.HasPrefix(entry, "GH_ENTERPRISE_TOKEN=") {
			kept = append(kept, entry)
		}
	}
	return kept
}
