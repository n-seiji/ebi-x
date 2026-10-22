package config

import (
	"context"
	"fmt"
	"log"
	"net/url"
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
	"~/.docker",
	"~/.kube",
	"~/.gnupg",
	"~/.netrc",
	"~/.git-credentials",
	"~/.config/git/credentials",
	"~/.npmrc",
	"~/.pypirc",
}

// pushExemption is a credential path that a repository's pushes depend on.
type pushExemption struct {
	path   string
	reason string
}

// resolveCredentialPaths returns the default credential stores to deny:
// those not already in protectedPaths, not listed in allowed, and not needed
// to push from one of the git repositories among writableRoots. allowed is
// EBIX_ALLOWED_READ_PATHS and affects only the default list.
func resolveCredentialPaths(writableRoots, protectedPaths []string, allowed string) ([]string, error) {
	exempt := make(map[string]struct{})
	for _, item := range splitList(allowed) {
		path, err := canonicalHomePath(item)
		if err != nil {
			return nil, fmt.Errorf("EBIX_ALLOWED_READ_PATHS %q: %w", item, err)
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
		exemptions, err := pushExemptions(ctx, root)
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

// pushExemptions returns the credential stores the repository at root needs
// to push to its origin from inside the sandbox.
func pushExemptions(ctx context.Context, root string) ([]pushExemption, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		// No origin, so nothing is pushed.
		return nil, nil
	}
	origin := strings.TrimSpace(string(out))
	if isSSHRemote(origin) {
		path, err := canonicalHomePath("~/.ssh")
		if err != nil {
			return nil, err
		}
		return []pushExemption{{path, "pushes to its origin over SSH"}}, nil
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil
	}
	// The origin may embed credentials, so drop them before anything is
	// logged or passed on.
	u.User = nil
	where := "pushes to its origin " + u.Host
	// --get-urlmatch reports only the last matching value, which is the one
	// the most specific configuration section ends with.
	helperOut, err := exec.CommandContext(ctx, "git", "-C", root, "config", "--get-urlmatch", "credential.helper", u.String()).Output()
	var helper string
	if err == nil {
		helper = strings.TrimSpace(string(helperOut))
	}
	var candidates []string
	var reason string
	switch {
	case helper == "":
		candidates, reason = []string{"~/.netrc"}, where+" with no credential helper"
	case isGHHelper(helper):
		candidates, reason = []string{"~/.config/gh"}, where+" with the gh credential helper"
	default:
		file, ok := storeHelperFile(helper)
		if !ok {
			return nil, nil
		}
		reason = where + " with the store credential helper"
		candidates = []string{"~/.git-credentials", "~/.config/git/credentials"}
		if file != "" {
			candidates = []string{file}
		}
	}
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
