// Package slackfmt prepares Markdown model output for delivery to Slack.
//
// Slack renders a Block Kit markdown block as standard Markdown, so the model
// output itself needs no rewriting. What it does need is to be cut into blocks
// Slack accepts without breaking the Markdown across the cut, and a plain-text
// rendering for the places Slack cannot show a block.
package slackfmt

import (
	"regexp"
	"strings"
)

// minimumSplitLimit is the smallest limit for which Split keeps track of code
// fences. Below it the fence bookkeeping would not reliably fit in a chunk, so
// Split degrades to plain rune chunks instead.
const minimumSplitLimit = 64

// Split breaks text into chunks of at most limit runes, cutting on line
// boundaries so headings, list items, and table rows survive the cut. A chunk
// that would end inside a fenced code block closes the fence, and the next
// chunk reopens it with the same marker, so neither half renders as prose.
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

// addOversized places a line that cannot fit in any single chunk, cutting it
// at rune boundaries. Fence state is tracked from the whole line, because the
// pieces are the same line as far as Markdown is concerned.
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

// cost is the runes a line adds to the current chunk, including the newline
// that joins it to the line before it.
func (s *splitter) cost(line string) int {
	cost := len([]rune(line))
	if len(s.lines) > 0 {
		cost++
	}
	return cost
}

// reserve is the room held back for the fence line that closes an open fence
// at the end of the current chunk.
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

// fenceMarker reports the fence character and how many times it is repeated at
// the start of line. A closing fence must use the same character as its
// opening fence and be at least as long.
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

// PlainText renders Markdown without its syntax. Slack shows it in notification
// previews and in clients that cannot render blocks, and the bot falls back to
// it when Slack rejects a block payload, so it keeps link targets rather than
// only their labels.
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
