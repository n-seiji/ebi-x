package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/n-seiji/ebi-x/internal/github"
)

// GitHubApp is the GitHub App whose installation tokens work turns use.
type GitHubApp struct {
	ID string
	// InstallationID is 0 when the App's only installation is meant.
	InstallationID int64
	// KeyLocation is where the PEM private key is: a file path, or
	// github.KeychainPrefix and a macOS Keychain service name.
	KeyLocation string
	// KeyPath is the canonical key file, hidden from Codex; empty for a
	// Keychain item.
	KeyPath string
}

// loadGitHubApp reads EBIX_GITHUB_APP_ID, EBIX_GITHUB_APP_PRIVATE_KEY, and
// EBIX_GITHUB_APP_INSTALLATION_ID. It returns nil when none is set.
func loadGitHubApp() (*GitHubApp, error) {
	id := strings.TrimSpace(os.Getenv("EBIX_GITHUB_APP_ID"))
	location := strings.TrimSpace(os.Getenv("EBIX_GITHUB_APP_PRIVATE_KEY"))
	installation := strings.TrimSpace(os.Getenv("EBIX_GITHUB_APP_INSTALLATION_ID"))
	if id == "" && location == "" && installation == "" {
		return nil, nil
	}
	if id == "" || location == "" {
		return nil, errors.New("EBIX_GITHUB_APP_ID and EBIX_GITHUB_APP_PRIVATE_KEY must be set together")
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, fmt.Errorf("EBIX_GITHUB_APP_ID %q: must be the App's numeric ID", id)
	}
	app := &GitHubApp{ID: id, KeyLocation: location}
	if installation != "" {
		value, err := strconv.ParseInt(installation, 10, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("EBIX_GITHUB_APP_INSTALLATION_ID %q: must be a positive number", installation)
		}
		app.InstallationID = value
	}
	if service, ok := strings.CutPrefix(location, github.KeychainPrefix); ok {
		if strings.TrimSpace(service) == "" {
			return nil, fmt.Errorf("EBIX_GITHUB_APP_PRIVATE_KEY %q: names no Keychain service", location)
		}
		return app, nil
	}
	expanded, err := expandHome(location)
	if err != nil {
		return nil, fmt.Errorf("EBIX_GITHUB_APP_PRIVATE_KEY %q: %w", location, err)
	}
	path, err := canonicalPath(expanded)
	if err != nil {
		return nil, fmt.Errorf("EBIX_GITHUB_APP_PRIVATE_KEY %q: %w", location, err)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("EBIX_GITHUB_APP_PRIVATE_KEY %q: %w", location, err)
	}
	app.KeyLocation, app.KeyPath = path, path
	return app, nil
}
