package github

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// revokeTimeout bounds the revocation that runs at the end of a turn, which
// delays the Slack reply.
const revokeTimeout = 10 * time.Second

const (
	// mintAttempts is how many times TurnEnv tries to mint a token.
	mintAttempts      = 3
	mintFirstBackoff  = time.Second
	mintSecondBackoff = 2 * time.Second
)

// credentialHelperScript answers git's credential get with the token stored
// beside it, and ignores store and erase so git cannot overwrite it.
const credentialHelperScript = `#!/bin/sh
test "$1" = get || exit 0
printf 'username=x-access-token\npassword=%s\n' "$(cat "$(dirname "$0")/token")"
`

// TurnEnv mints a token for one turn and writes it where git and gh run by
// the turn find it. It returns the variables that point them there and a
// function that revokes the token and removes it, so the token lives only
// as long as the turn. The variables avoid the names Codex keeps from
// commands (KEY, SECRET, TOKEN): GH_CONFIG_DIR selects a gh configuration
// holding the token, and GIT_CONFIG_PARAMETERS replaces git's credential
// helpers for the host with one that prints it. The host's own helpers, and
// the App private key they may need, are then never used inside the turn.
// A transient failure to mint is retried first.
func (a *AppTokens) TurnEnv(ctx context.Context) ([]string, func(), error) {
	token, err := a.mintWithRetry(ctx)
	if err != nil {
		return nil, nil, err
	}
	revoke := func() {
		// The turn's context may already be cancelled.
		revokeCtx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		defer cancel()
		if err := a.Revoke(revokeCtx, token); err != nil {
			log.Printf("%v; it expires within an hour", err)
		}
	}
	dir, err := os.MkdirTemp(a.dir, "turn-")
	if err != nil {
		revoke()
		return nil, nil, fmt.Errorf("create turn GitHub credential directory: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("remove turn GitHub credential directory: %v", err)
		}
		revoke()
	}
	env, err := writeTurnCredentials(dir, a.webHost, token)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return env, cleanup, nil
}

// mintWithRetry calls Mint up to mintAttempts times, waiting between
// attempts, while it fails transiently. It returns promptly when ctx ends.
func (a *AppTokens) mintWithRetry(ctx context.Context) (string, error) {
	for attempt := 1; ; attempt++ {
		token, err := a.Mint(ctx)
		if err == nil {
			return token, nil
		}
		if attempt == mintAttempts || !isTransient(err) {
			return "", err
		}
		wait := a.mintBackoff[attempt-1]
		log.Printf("mint GitHub App installation token: attempt %d of %d failed, retrying in %s: %v", attempt, mintAttempts, wait, err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("mint GitHub App installation token: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func writeTurnCredentials(dir, host, token string) ([]string, error) {
	ghDir := filepath.Join(dir, "gh")
	if err := os.Mkdir(ghDir, 0o700); err != nil {
		return nil, fmt.Errorf("create turn gh configuration: %w", err)
	}
	// gh rewrites a hosts.yml without users, or a config.yml without
	// version, to its multi-account format, which needs the API and
	// rewriting the files; the read-only turn directory prevents that, so
	// both are written in that format already.
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
	// git appends the operation to a "!" helper and runs it through the
	// shell, so the path is quoted for it too; unprefixed, a quoted path
	// would not read as absolute.
	parameters := shellQuote(key+"=") + " " + shellQuote(key+"=!"+shellQuote(helper))
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

// PrepareTurnDir creates dir for the turns' credentials, private to the bot,
// and removes what a crashed run left in it. The leftover tokens are
// short-lived, so a failed removal is only logged.
func PrepareTurnDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create turn GitHub credentials directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("list %s for stale turn credentials: %v", dir, err)
		return nil
	}
	for _, entry := range entries {
		stale := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(stale); err != nil {
			log.Printf("remove stale turn credentials %s: %v", stale, err)
			continue
		}
		log.Printf("removed stale turn credentials %s", stale)
	}
	return nil
}
