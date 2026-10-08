package codex

import (
	"errors"
	"strings"
)

const (
	policyHeading              = "## 方針"
	instructionHeading         = "## 作業指示"
	memoryHeading              = "## メモリ追記"
	globalMemoryHeading        = "## 全体メモリ追記"
	channelMemoryHeading       = "## チャンネルメモリ追記"
	forbiddenUserMemoryHeading = "## ユーザーメモリ追記"
	attachmentsHeading         = "## 添付ファイル"
)

// MemoryAppends contains optional entries proposed by a work turn. The bot
// chooses the actual channel path from the authenticated Slack event.
type MemoryAppends struct {
	Global  string
	Channel string
}

// ParsePlan extracts the policy and work instruction from a plan response.
func ParsePlan(text string) (string, string, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var policyIndexes, instructionIndexes []int
	prose := proseLines(lines)

	for i, line := range lines {
		if !prose[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case policyHeading:
			policyIndexes = append(policyIndexes, i)
		case instructionHeading:
			instructionIndexes = append(instructionIndexes, i)
		}
	}

	if len(policyIndexes) != 1 || len(instructionIndexes) != 1 {
		return "", "", errors.New("plan must contain exactly one policy and one instruction heading")
	}
	policyIndex := policyIndexes[0]
	instructionIndex := instructionIndexes[0]
	if policyIndex >= instructionIndex {
		return "", "", errors.New("policy heading must precede instruction heading")
	}

	policy := strings.TrimSpace(strings.Join(lines[policyIndex+1:instructionIndex], "\n"))
	instruction := strings.TrimSpace(strings.Join(lines[instructionIndex+1:], "\n"))
	if policy == "" || instruction == "" {
		return "", "", errors.New("plan sections must not be empty")
	}
	if instruction == "NONE" {
		instruction = ""
	}
	return policy, instruction, nil
}

// SanitizeSlackOutput removes the retired user-memory section and everything
// after it wherever the exact heading appears in model output. Truncating at
// the first occurrence prevents fenced, inline, and malformed variants from
// exposing the following private payload while preserving prior output.
func SanitizeSlackOutput(text string) string {
	index := strings.Index(text, forbiddenUserMemoryHeading)
	if index < 0 {
		return text
	}
	return strings.TrimSpace(text[:index])
}

// SplitMemoryAppends separates optional trailing scoped memory sections from
// a work response. Sections must occur at most once and in global, channel
// order. The legacy generic heading is treated as global memory. Ambiguous
// output causes no memory writes and is stripped from the visible result.
func SplitMemoryAppends(text string) (rest string, appends MemoryAppends, valid bool) {
	if strings.Contains(text, forbiddenUserMemoryHeading) {
		return SanitizeSlackOutput(text), MemoryAppends{}, false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	type section struct {
		index int
		scope int
	}
	var sections []section
	firstMemoryIndex := -1
	prose := proseLines(lines)

	for i, line := range lines {
		if !prose[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		scope := -1
		switch trimmed {
		case memoryHeading, globalMemoryHeading:
			scope = 0
		case channelMemoryHeading:
			scope = 1
		}
		if scope >= 0 {
			if firstMemoryIndex == -1 {
				firstMemoryIndex = i
			}
			sections = append(sections, section{index: i, scope: scope})
		}
	}

	if len(sections) == 0 {
		return text, MemoryAppends{}, true
	}
	result := strings.TrimSpace(strings.Join(lines[:sections[0].index], "\n"))
	lastScope := -1
	for _, section := range sections {
		if section.scope <= lastScope {
			return result, MemoryAppends{}, false
		}
		lastScope = section.scope
	}

	for i, section := range sections {
		end := len(lines)
		if i+1 < len(sections) {
			end = sections[i+1].index
		}
		entry := strings.TrimSpace(strings.Join(lines[section.index+1:end], "\n"))
		switch section.scope {
		case 0:
			appends.Global = entry
		case 1:
			appends.Channel = entry
		}
	}
	return result, appends, true
}

// SplitMemoryAppend preserves the original single-global-memory API.
func SplitMemoryAppend(text string) (rest, entry string) {
	rest, appends, valid := SplitMemoryAppends(text)
	if !valid || appends.Channel != "" {
		return text, ""
	}
	return rest, appends.Global
}

// SplitAttachments removes the attachment section from a work response. The
// section runs from its heading to the next level-2 heading or the end, so it
// may sit before or after the memory sections, and lists one file path per
// bullet line. It must occur at most once. Malformed output yields no
// attachments and reports valid as false, so the bot can tell the user
// instead of guessing which files were meant.
func SplitAttachments(text string) (rest string, paths []string, valid bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start, end := -1, len(lines)
	sections := 0
	prose := proseLines(lines)

	for i, line := range lines {
		if !prose[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == attachmentsHeading {
			sections++
			if start == -1 {
				start = i
			}
		} else if start != -1 && end == len(lines) && strings.HasPrefix(trimmed, "## ") {
			end = i
		}
	}

	if sections == 0 {
		return text, nil, true
	}
	kept := append(append([]string(nil), lines[:start]...), lines[end:]...)
	rest = strings.TrimSpace(strings.Join(kept, "\n"))
	if sections > 1 {
		return rest, nil, false
	}
	for _, line := range lines[start+1 : end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "- ")
		if !ok {
			return rest, nil, false
		}
		item = strings.TrimSpace(item)
		if unquoted, ok := strings.CutPrefix(item, "`"); ok {
			if item, ok = strings.CutSuffix(unquoted, "`"); !ok {
				return rest, nil, false
			}
		}
		if item == "" {
			return rest, nil, false
		}
		paths = append(paths, item)
	}
	return rest, paths, true
}

// proseLines reports, for each line, whether it is outside fenced code
// blocks and is not a fence line itself, so headings quoted in code are not
// taken as output-contract sections.
func proseLines(lines []string) []bool {
	prose := make([]bool, len(lines))
	inFence := false
	var fence byte
	var fenceLen int
	for i, line := range lines {
		if marker, length, ok := fenceMarker(strings.TrimSpace(line)); ok {
			if !inFence {
				inFence = true
				fence = marker
				fenceLen = length
			} else if marker == fence && length >= fenceLen {
				inFence = false
			}
			continue
		}
		prose[i] = !inFence
	}
	return prose
}

// fenceMarker reports the fence character and the number of times it is
// repeated at the start of line. A closing fence must use the same character
// as its opening fence and be at least as long.
func fenceMarker(line string) (char byte, length int, ok bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	char = line[0]
	for length < len(line) && line[length] == char {
		length++
	}
	if length < 3 {
		return 0, 0, false
	}
	return char, length, true
}
