package github

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// PushToken returns the password git uses to push to remote, an https
// remote on GitHub: the token the work turns already push with, read
// through git's own credential helpers. It never prompts. An SSH remote has
// no such token.
func PushToken(ctx context.Context, remote string) (string, error) {
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("remote is not an https URL")
	}
	// Ask as a checkout does: outside any repository, so only the global
	// and system git configuration apply.
	command := exec.CommandContext(ctx, "git", "credential", "fill")
	command.Dir = os.TempDir()
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	command.Stdin = strings.NewReader("url=" + remote + "\n\n")
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		return "", err
	}
	return parseCredential(stdout.String())
}

func parseCredential(output string) (string, error) {
	for line := range strings.Lines(output) {
		if password, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "password="); ok && password != "" {
			return password, nil
		}
	}
	return "", errors.New("git has no stored credential for the remote")
}
