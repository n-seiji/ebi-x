// Package slackfmt does not rewrite model output: Slack renders a Block Kit
// markdown block as standard Markdown.
package slackfmt

import (
	"regexp"
	"strings"
)

// Below it the fence bookkeeping would not reliably fit in a chunk, so Split
// degrades to plain rune chunks.
const minimumSplitLimit = 64

func Split(text string, limit int) []string {
	if limit <= 0 || len([]rune(text)) <= limit {
		return []string{text}
	}
	if limit < minimumSplitLimit {
		return runeChunks(text, limit)
	}
	splitter := &splitter{limit: limit}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		splitter.add(line)
	}
	return splitter.done()
}

type splitter struct {
	limit   int
	chunks  []string
	lines   []string
	length  int
	open    string // opening fence line repeated when a chunk cuts a fence
	closing string // fence line that closes open
	marker  byte
	width   int
}

func (s *splitter) add(line string) {
	if s.length+s.cost(line)+s.reserve() > s.limit && len(s.lines) > 0 {
		s.flush()
	}
	if s.length+s.cost(line)+s.reserve() > s.limit {
		s.addOversized(line)
		return
	}
	s.append(line)
	s.track(line)
}

// Fence state is tracked from the whole line, because the pieces are the same
// line as far as Markdown is concerned.
func (s *splitter) addOversized(line string) {
	runes := []rune(line)
	for len(runes) > 0 {
		room := s.limit - s.length - s.reserve()
		if len(s.lines) > 0 {
			room-- // the newline joining this piece to the previous line
		}
		if room <= 0 {
			s.flush()
			continue
		}
		taken := min(len(runes), room)
		s.append(string(runes[:taken]))
		runes = runes[taken:]
		if len(runes) > 0 {
			s.flush()
		}
	}
	s.track(line)
}

func (s *splitter) done() []string {
	// A cut that lands on the end of the message leaves a reopened fence with
	// nothing left to fence.
	if s.open != "" && len(s.lines) == 1 && s.lines[0] == s.open {
		s.lines = nil
		s.length = 0
	}
	s.open = "" // the final chunk ends the message, so nothing is reopened
	s.flush()
	if len(s.chunks) == 0 {
		return []string{""}
	}
	return s.chunks
}

func (s *splitter) append(line string) {
	s.length += s.cost(line)
	s.lines = append(s.lines, line)
}

func (s *splitter) cost(line string) int {
	cost := len([]rune(line))
	if len(s.lines) > 0 {
		cost++
	}
	return cost
}

func (s *splitter) reserve() int {
	if s.open == "" {
		return 0
	}
	return len([]rune(s.closing)) + 1
}

func (s *splitter) flush() {
	if len(s.lines) == 0 && s.open == "" {
		return
	}
	chunk := strings.Join(s.lines, "\n")
	if s.open != "" {
		chunk += "\n" + s.closing
	}
	s.chunks = append(s.chunks, chunk)
	s.lines = nil
	s.length = 0
	if s.open == "" {
		return
	}
	// Reopening costs room in every following chunk. When a fence is long
	// enough for that to crowd out the content, stop tracking it and let the
	// rest of the message render as prose rather than emit useless chunks.
	if 4*(len([]rune(s.open))+1+s.reserve()) > s.limit {
		s.clearFence()
		return
	}
	s.append(s.open)
}

func (s *splitter) track(line string) {
	marker, width, ok := fenceMarker(strings.TrimSpace(line))
	if !ok {
		return
	}
	if s.open == "" {
		s.open = line
		s.closing = strings.Repeat(string(marker), width)
		s.marker = marker
		s.width = width
		return
	}
	if marker == s.marker && width >= s.width {
		s.clearFence()
	}
}

func (s *splitter) clearFence() {
	s.open = ""
	s.closing = ""
	s.marker = 0
	s.width = 0
}

// A closing fence must use the same character as its opening fence and be at
// least as long.
func fenceMarker(line string) (marker byte, width int, ok bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	marker = line[0]
	for width < len(line) && line[width] == marker {
		width++
	}
	if width < 3 {
		return 0, 0, false
	}
	return marker, width, true
}

func runeChunks(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	chunks := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		n := min(len(runes), limit)
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}

var (
	linkPattern    = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)
	headingPattern = regexp.MustCompile(`(?m)^ {0,3}#{1,6}[ \t]+`)
	emphasisMarks  = strings.NewReplacer("**", "", "__", "", "`", "")
)

// Keeps link targets rather than only their labels: the bot falls back to it
// when Slack rejects a block payload.
func PlainText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = linkPattern.ReplaceAllString(text, "$1 ($2)")
	text = headingPattern.ReplaceAllString(text, "")
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if _, _, ok := fenceMarker(strings.TrimSpace(line)); ok {
			continue
		}
		kept = append(kept, emphasisMarks.Replace(line))
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// The model sometimes writes Slack's link syntax even though the bot posts
// Markdown.
var slackLinkPattern = regexp.MustCompile(`<([^<>|\s]+)\|([^<>]*)>`)

// A path under home is shown relative to home, so the operator can still find
// it without the machine's home directory showing.
func HideLocalPaths(text, home string) string {
	text = linkPattern.ReplaceAllStringFunc(text, func(link string) string {
		match := linkPattern.FindStringSubmatch(link)
		if !localTarget(match[2]) {
			return link
		}
		return match[1]
	})
	text = slackLinkPattern.ReplaceAllStringFunc(text, func(link string) string {
		match := slackLinkPattern.FindStringSubmatch(link)
		if !localTarget(match[1]) {
			return link
		}
		return match[2]
	})
	home = strings.TrimRight(home, "/")
	if home == "" {
		return text
	}
	return strings.ReplaceAll(text, home+"/", "")
}

func localTarget(target string) bool {
	switch {
	case strings.HasPrefix(target, "//"):
		return false
	case strings.HasPrefix(target, "/"), strings.HasPrefix(target, "~/"), strings.HasPrefix(target, "file:"):
		return true
	}
	return false
}
