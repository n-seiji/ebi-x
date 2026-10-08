// Package playbook discovers the playbooks available to ebi-x.
package playbook

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxFileSize = 64 * 1024

// Playbook describes a playbook that Codex may use.
type Playbook struct {
	Name        string
	Description string
	Path        string
}

// List returns the valid Markdown playbooks directly contained in dir.
func List(dir string) ([]Playbook, error) {
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve playbooks directory: %w", err)
	}

	entries, err := os.ReadDir(absoluteDir)
	if err != nil {
		return nil, fmt.Errorf("read playbooks directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	playbooks := make([]Playbook, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		// README.md は playbook 置き場の説明用であり playbook ではない。
		if strings.EqualFold(entry.Name(), "README.md") {
			continue
		}

		path := filepath.Join(absoluteDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat playbook %q: %w", path, err)
		}
		if info.Size() > maxFileSize {
			log.Printf("warning: ignoring playbook %q: file exceeds 64KB", path)
			continue
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read playbook %q: %w", path, err)
		}
		if len(content) > maxFileSize {
			log.Printf("warning: ignoring playbook %q: file exceeds 64KB", path)
			continue
		}
		name, description, ok := parseFrontmatter(content)
		if !ok {
			log.Printf("warning: ignoring playbook %q: missing or incomplete frontmatter", path)
			continue
		}
		playbooks = append(playbooks, Playbook{
			Name:        name,
			Description: description,
			Path:        path,
		})
	}

	return playbooks, nil
}

func parseFrontmatter(content []byte) (string, string, bool) {
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	lines := strings.Split(string(content), "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return "", "", false
	}

	var name, description string
	closed := false
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			description = strings.TrimSpace(value)
		}
	}
	if !closed || name == "" || description == "" {
		return "", "", false
	}
	return name, description, true
}
