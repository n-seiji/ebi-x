package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/n-seiji/ebi-x/internal/github"
)

type GitHubApp struct {
	ID             string
	InstallationID int64
	KeyLocation    string
	KeyPath        string
}

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
