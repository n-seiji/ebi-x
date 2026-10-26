package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// credentialHelperScript answers git's credential get with the token stored
// beside it, and ignores store and erase so git cannot overwrite it.
const credentialHelperScript = `#!/bin/sh
test "$1" = get || exit 0
printf 'username=x-access-token\npassword=%s\n' "$(cat "$(dirname "$0")/token")"
`

// TurnEnv writes an installation token where git and gh run by a Codex turn
// find it, and returns the variables that point them there and a function
// that removes it. The variables avoid the names Codex keeps from commands
// (KEY, SECRET, TOKEN): GH_CONFIG_DIR selects a gh configuration holding
// the token, and GIT_CONFIG_PARAMETERS replaces git's credential helpers for
// the host with one that prints it. The host's own helpers, and the App
// private key they may need, are then never used inside the turn.
func (a *AppTokens) TurnEnv(ctx context.Context) ([]string, func(), error) {
	token, err := a.Token(ctx)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "ebi-x-github-")
	if err != nil {
		return nil, nil, fmt.Errorf("create turn GitHub credential directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	env, err := writeTurnCredentials(dir, a.webHost, token)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return env, cleanup, nil
}

func writeTurnCredentials(dir, host, token string) ([]string, error) {
	ghDir := filepath.Join(dir, "gh")
	if err := os.Mkdir(ghDir, 0o700); err != nil {
		return nil, fmt.Errorf("create turn gh configuration: %w", err)
	}
	// gh rewrites a hosts.yml without users, or a config.yml without
	// version, to its multi-account format, which needs the API and a
	// writable directory; the sandbox gives the turn neither, so both are
	// written in that format already.
	hosts := fmt.Sprintf("%s:\n    users:\n        x-access-token:\n            oauth_token: %s\n    git_protocol: https\n    oauth_token: %s\n    user: x-access-token\n", host, token, token)
	if err := os.WriteFile(filepath.Join(ghDir, "config.yml"), []byte("version: \"1\"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write turn gh configuration: %w", err)
	}
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		return nil, fmt.Errorf("write turn gh configuration: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("write turn GitHub token: %w", err)
	}
	helper := filepath.Join(dir, "git-credential-ebi-x")
	if err := os.WriteFile(helper, []byte(credentialHelperScript), 0o700); err != nil {
		return nil, fmt.Errorf("write turn git credential helper: %w", err)
	}
	// An empty helper clears those configured before it, and command-line
	// configuration is read last, so only the turn's helper answers for the
	// host. The single-quoted 'key=value' form is the one every git release
	// that reads GIT_CONFIG_PARAMETERS accepts.
	key := "credential.https://" + host + ".helper"
	parameters := shellQuote(key+"=") + " " + shellQuote(key+"="+helper)
	return []string{
		"GH_CONFIG_DIR=" + ghDir,
		"GH_HOST=" + host,
		"GIT_CONFIG_PARAMETERS=" + parameters,
	}, nil
}

// shellQuote quotes value the way git's sq_quote does.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
