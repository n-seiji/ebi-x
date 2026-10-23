package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/n-seiji/ebi-x/internal/workspace"
)

// credentialPaths are well-known credential stores, relative to the home
// directory, that the agent cannot read unless a push needs them or
// EBIX_ALLOWED_READ_PATHS says so.
var credentialPaths = []string{
	"~/.ssh",
	"~/.aws",
	"~/.azure",
	"~/.config/gcloud",
	"~/.config/gh",
	"~/.docker/config.json",
	"~/.kube",
	"~/.gnupg",
	"~/.netrc",
	"~/.git-credentials",
	"~/.config/git/credentials",
	"~/.npmrc",
	"~/.pypirc",
	"~/.config/op",
	"~/.vault-token",
	"~/.terraform.d/credentials.tfrc.json",
	"~/.cargo/credentials.toml",
	"~/.gem/credentials",
	"~/.pgpass",
	"~/.my.cnf",
	"~/.s3cfg",
	"~/.boto",
	"~/.oci",
	"~/.config/rclone",
	"~/.claude",
	"~/.config/github-copilot",
}

// pushExemption is a credential path that a repository's pushes depend on.
type pushExemption struct {
	path   string
	reason string
}

// resolveCredentialPaths returns the default credential stores to deny:
// those not already in protectedPaths, not listed in allowed, and not needed
// to push or sign from one of the git repositories among writableRoots.
// allowed is EBIX_ALLOWED_READ_PATHS and affects only the default list.
// checkoutsDir is where thread checkouts are created; push credentials are
// detected from a scratch repository there, which sees the same git
// configuration (global, system, and includeIf) a thread checkout will.
func resolveCredentialPaths(writableRoots, protectedPaths []string, allowed, checkoutsDir string) ([]string, error) {
	known := make(map[string]struct{}, len(credentialPaths))
	for _, item := range credentialPaths {
		path, err := canonicalHomePath(item)
		if err != nil {
			return nil, fmt.Errorf("resolve credential path %q: %w", item, err)
		}
		known[path] = struct{}{}
	}
	exempt := make(map[string]struct{})
	for _, item := range splitList(allowed) {
		path, err := canonicalHomePath(item)
		if err != nil {
			return nil, fmt.Errorf("EBIX_ALLOWED_READ_PATHS %q: %w", item, err)
		}
		if _, ok := known[path]; !ok {
			return nil, fmt.Errorf("EBIX_ALLOWED_READ_PATHS %q is not a default credential path; valid entries: %s", item, strings.Join(credentialPaths, ", "))
		}
		exempt[path] = struct{}{}
	}
	ctx := context.Background()
	for _, root := range writableRoots {
		repo, err := workspace.IsRepositoryTopLevel(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("inspect writable root %q: %w", root, err)
		}
		if !repo {
			continue
		}
		exemptions, err := pushExemptions(ctx, root, checkoutsDir)
		if err != nil {
			return nil, fmt.Errorf("detect push credentials of %q: %w", root, err)
		}
		for _, e := range exemptions {
			if _, done := exempt[e.path]; !done {
				log.Printf("allowing Codex to read %s: %s %s", e.path, root, e.reason)
			}
			exempt[e.path] = struct{}{}
		}
	}

	var paths []string
	for _, item := range credentialPaths {
		path, err := canonicalHomePath(item)
		if err != nil {
			return nil, fmt.Errorf("resolve credential path %q: %w", item, err)
		}
		if _, ok := exempt[path]; ok || slices.Contains(protectedPaths, path) || slices.Contains(paths, path) {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func canonicalHomePath(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	return canonicalPath(expanded)
}

// pushExemptions returns the credential stores a thread checkout of the
// repository at root needs to push to its origin and sign commits from inside
// the sandbox.
//
// A checkout is a fresh clone: it inherits neither the original repository's
// local config nor its includeIf matches, only the origin URL the workspace
// manager copies over. So the detection runs in a scratch repository under
// checkoutsDir with that same origin URL, where git reads the same global and
// system configuration a checkout will.
func pushExemptions(ctx context.Context, root, checkoutsDir string) ([]pushExemption, error) {
	origin, err := workspace.OriginURL(ctx, root)
	if err != nil {
		log.Printf("credential detection: read origin of %s: %v", root, err)
	}
	if err := os.MkdirAll(checkoutsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create checkout directory: %w", err)
	}
	removeStaleProbes(checkoutsDir)
	probe, err := os.MkdirTemp(checkoutsDir, probePrefix)
	if err != nil {
		return nil, fmt.Errorf("create credential probe: %w", err)
	}
	defer os.RemoveAll(probe)
	if _, err := gitConfigOutput(ctx, probe, "init", "-q"); err != nil {
		return nil, err
	}

	var exemptions []pushExemption
	if origin != "" {
		if _, err := gitConfigOutput(ctx, probe, "config", "remote.origin.url", origin); err != nil {
			return nil, err
		}
		found, err := originExemptions(ctx, root, probe)
		if err != nil {
			return nil, err
		}
		exemptions = append(exemptions, found...)
	}
	found, err := signingExemptions(ctx, root, probe)
	if err != nil {
		return nil, err
	}
	return append(exemptions, found...), nil
}

// probePrefix names the scratch repositories credential detection creates.
const probePrefix = ".credprobe-"

// removeStaleProbes deletes scratch repositories a killed earlier run left in
// checkoutsDir, logging each. Failures are logged and otherwise ignored: a
// leftover is clutter, not a reason to refuse to start.
func removeStaleProbes(checkoutsDir string) {
	entries, err := os.ReadDir(checkoutsDir)
	if err != nil {
		log.Printf("credential detection: list %s for stale probes: %v", checkoutsDir, err)
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), probePrefix) {
			continue
		}
		stale := filepath.Join(checkoutsDir, entry.Name())
		if err := os.RemoveAll(stale); err != nil {
			log.Printf("credential detection: remove stale probe %s: %v", stale, err)
			continue
		}
		log.Printf("credential detection: removed stale probe %s", stale)
	}
}

// gitConfigOutput runs git in dir and returns its stdout.
func gitConfigOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), text, err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// gitConfigValues returns the values git prints for a config query, one per
// line, and none when the key is not set (exit status 1).
func gitConfigValues(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := gitConfigOutput(ctx, dir, append([]string{"config"}, args...)...)
	if err != nil {
		if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n"), nil
}

// originExemptions returns the credential stores pushing to the probe's
// origin needs.
func originExemptions(ctx context.Context, root, probe string) ([]pushExemption, error) {
	out, err := gitConfigOutput(ctx, probe, "remote", "get-url", "--push", "origin")
	if err != nil {
		log.Printf("credential detection: resolve the push URL of %s: %v", root, err)
		return nil, nil
	}
	pushURL := strings.TrimSpace(out)
	if isSSHRemote(pushURL) {
		path, err := canonicalHomePath("~/.ssh")
		if err != nil {
			return nil, err
		}
		return []pushExemption{{path, "pushes to its origin over SSH"}}, nil
	}
	u, err := url.Parse(pushURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil
	}
	// The origin may embed credentials, so drop them before anything is
	// logged or passed on.
	u.User = nil
	where := "pushes to its origin " + u.Host

	helpers, err := helperChain(ctx, probe)
	if err != nil {
		log.Printf("credential detection: read credential helpers of %s: %v", root, err)
		return nil, nil
	}
	var candidates, unrecognized []string
	var reasons []string
	for _, helper := range helpers {
		switch file, isStore := storeHelperFile(helper); {
		case isGHHelper(helper):
			candidates = append(candidates, "~/.config/gh")
			reasons = append(reasons, "the gh credential helper")
		case isStore:
			reasons = append(reasons, "the store credential helper")
			if file != "" {
				candidates = append(candidates, file)
			} else {
				candidates = append(candidates, "~/.git-credentials", "~/.config/git/credentials")
			}
		default:
			unrecognized = append(unrecognized, helperLabel(helper))
		}
	}
	if len(helpers) == 0 {
		candidates, reasons = []string{"~/.netrc"}, []string{"no credential helper"}
	}
	if len(candidates) == 0 {
		log.Printf("credential detection: %s %s with credential helper %s; no credential path needs allowing",
			root, where, strings.Join(unrecognized, ", "))
		return nil, nil
	}
	reason := where + " with " + strings.Join(slices.Compact(reasons), " and ")
	var exemptions []pushExemption
	for _, c := range candidates {
		path, err := canonicalHomePath(c)
		if err != nil {
			return nil, err
		}
		exemptions = append(exemptions, pushExemption{path, reason})
	}
	return exemptions, nil
}

// helperChain returns every credential helper configured anywhere in the
// probe's git configuration: credential.helper and each
// credential.<url>.helper, whatever URL they are scoped to. git consults only
// the sections matching the push URL, but the exact URL matching (ports,
// paths, wildcards, pushInsteadOf rewrites) is git's to get right, not ours, so
// this takes the union and may over-exempt, which only exposes more and never
// breaks a push. Empty values, which reset the list at the point they appear,
// are ignored for the same reason: honoring one could drop a helper the
// remaining sections still call.
func helperChain(ctx context.Context, probe string) ([]string, error) {
	lines, err := gitConfigValues(ctx, probe, "--get-regexp", `^credential\..*helper$`)
	if err != nil {
		return nil, err
	}
	var chain []string
	for _, line := range lines {
		_, value, _ := strings.Cut(line, " ")
		if value = strings.TrimSpace(value); value != "" && !slices.Contains(chain, value) {
			chain = append(chain, value)
		}
	}
	return chain, nil
}

// helperLabel names a credential helper for logs without echoing arguments,
// which may hold secrets.
func helperLabel(helper string) string {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(helper), "!"))
	if len(fields) == 0 {
		return "?"
	}
	return filepath.Base(fields[0])
}

// signingExemptions returns the credential stores commit signing needs.
func signingExemptions(ctx context.Context, root, probe string) ([]pushExemption, error) {
	signing, err := gitConfigValues(ctx, probe, "--type=bool", "--get", "commit.gpgsign")
	if err != nil {
		log.Printf("credential detection: read commit.gpgsign of %s: %v", root, err)
		return nil, nil
	}
	if len(signing) == 0 || signing[len(signing)-1] != "true" {
		return nil, nil
	}
	format, err := gitConfigValues(ctx, probe, "--get", "gpg.format")
	if err != nil {
		log.Printf("credential detection: read gpg.format of %s: %v", root, err)
		return nil, nil
	}
	var path, reason string
	switch f := strings.ToLower(strings.TrimSpace(strings.Join(format, ""))); f {
	case "ssh":
		path, reason = "~/.ssh", "signs commits with an SSH key"
	case "", "openpgp":
		path, reason = "~/.gnupg", "signs commits with GnuPG"
	default:
		// x509 signing goes through gpgsm and needs no default credential path.
		return nil, nil
	}
	resolved, err := canonicalHomePath(path)
	if err != nil {
		return nil, err
	}
	return []pushExemption{{resolved, reason}}, nil
}

// isSSHRemote reports whether a git remote URL pushes over SSH, including
// the scp-like user@host:path form.
func isSSHRemote(remote string) bool {
	if scheme, _, ok := strings.Cut(remote, "://"); ok {
		return scheme == "ssh" || scheme == "git+ssh" || scheme == "ssh+git"
	}
	colon := strings.Index(remote, ":")
	return colon > 0 && !strings.Contains(remote[:colon], "/")
}

// isGHHelper reports whether a credential helper runs the GitHub CLI.
func isGHHelper(helper string) bool {
	for field := range strings.FieldsSeq(helper) {
		if filepath.Base(strings.TrimPrefix(field, "!")) == "gh" {
			return true
		}
	}
	return false
}

// storeHelperFile reports whether helper is git's store helper and returns
// its --file argument, empty for the default locations.
func storeHelperFile(helper string) (string, bool) {
	fields := strings.Fields(helper)
	if len(fields) == 0 || (fields[0] != "store" && filepath.Base(fields[0]) != "git-credential-store") {
		return "", false
	}
	for i, field := range fields[1:] {
		if value, ok := strings.CutPrefix(field, "--file="); ok {
			return value, true
		}
		if field == "--file" && i+2 < len(fields) {
			return fields[i+2], true
		}
	}
	return "", true
}
