package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPushToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	store := filepath.Join(home, "credentials")
	if err := os.WriteFile(store, []byte("https://x-access-token:secret@github.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(config, []byte("[credential]\n\thelper = store --file="+store+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	token, err := PushToken(context.Background(), "https://github.com/o/r.git")
	if err != nil || token != "secret" {
		t.Fatalf("PushToken() = %q, %v; want secret", token, err)
	}
	if _, err := PushToken(context.Background(), "https://example.com/o/r.git"); err == nil {
		t.Fatal("PushToken(unknown host) error = nil, want an error")
	}
	if _, err := PushToken(context.Background(), "git@github.com:o/r.git"); err == nil {
		t.Fatal("PushToken(ssh) error = nil, want an error")
	}
}

func TestParseCredential(t *testing.T) {
	if got, err := parseCredential("protocol=https\nhost=github.com\nusername=u\npassword=p\n"); err != nil || got != "p" {
		t.Fatalf("parseCredential() = %q, %v", got, err)
	}
	if _, err := parseCredential("protocol=https\nhost=github.com\n"); err == nil {
		t.Fatal("parseCredential(no password) error = nil")
	}
}
