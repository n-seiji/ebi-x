package slackbot

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
)

// maxActionRulesBytes bounds the operator's rules file, which every new
// thread's prompt carries.
const maxActionRulesBytes = 16 << 10

// readActionRules returns the operator's action rules, or "" for the
// built-in rules when the file is missing or unreadable. It is read for each
// new thread, so edits apply without a restart.
func readActionRules(path string) string {
	if path == "" {
		return ""
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		log.Printf("slackbot: read action rules %q: %v; using the built-in rules", path, err)
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxActionRulesBytes+1))
	if err != nil {
		log.Printf("slackbot: read action rules %q: %v; using the built-in rules", path, err)
		return ""
	}
	if len(data) > maxActionRulesBytes {
		log.Printf("slackbot: action rules %q exceed %d bytes; using the built-in rules", path, maxActionRulesBytes)
		return ""
	}
	return string(data)
}
