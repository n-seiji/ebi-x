// Package policy is compiled into the binary so it cannot be modified from
// Slack, memory, or the sandboxed filesystem.
package policy

import (
	_ "embed"
	"strings"
)

//go:embed policy.md
var instructions string

func Instructions() string {
	return strings.TrimSpace(instructions)
}
