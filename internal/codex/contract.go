package codex

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Follow-up limits. The prompt states them and the bot enforces them.
const (
	FollowUpMinDelay = 5 * time.Minute
	FollowUpMaxDelay = 7 * 24 * time.Hour
	// MaxFollowUpChain is how many follow-ups may be scheduled in a row
	// without a person writing in the thread.
	MaxFollowUpChain = 10
)

// FollowUpRange describes the accepted follow-up times, for the prompt and
// for the notice that rejects a time.
func FollowUpRange() string {
	minimum := strings.TrimSuffix(FollowUpMinDelay.String(), "0s")
	return fmt.Sprintf("%s後から%d日後まで", minimum, int(FollowUpMaxDelay/(24*time.Hour)))
}

// FollowUpDueAt reads a follow-up's "いつ": a delay such as "30m", "2h", or
// "1d", or an RFC 3339 time. It fails outside FollowUpRange.
func FollowUpDueAt(when string, now time.Time) (time.Time, error) {
	when = strings.TrimSpace(when)
	var dueAt time.Time
	if days, ok := strings.CutSuffix(when, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse days: %w", err)
		}
		dueAt = now.Add(time.Duration(count) * 24 * time.Hour)
	} else if delay, err := time.ParseDuration(when); err == nil {
		dueAt = now.Add(delay)
	} else if at, err := time.Parse(time.RFC3339, when); err == nil {
		dueAt = at
	} else {
		return time.Time{}, errors.New("not a delay or RFC 3339 time")
	}
	if delay := dueAt.Sub(now); delay < FollowUpMinDelay || delay > FollowUpMaxDelay {
		return time.Time{}, fmt.Errorf("delay %v is out of range", delay)
	}
	return dueAt, nil
}

const (
	memoryHeading              = "## メモリ追記"
	globalMemoryHeading        = "## 全体メモリ追記"
	channelMemoryHeading       = "## チャンネルメモリ追記"
	forbiddenUserMemoryHeading = "## ユーザーメモリ追記"
	attachmentsHeading         = "## 添付ファイル"
	followUpHeading            = "## フォローアップ"
	followUpWhenKey            = "いつ"
	followUpTaskKey            = "やること"
	questionsHeading           = "## 回答待ち"
	pullWatchHeading           = "## PR の見守り"
	// CheckoutRequestHeading asks the bot to prepare the thread's checkouts
	// and continue the same session in them.
	CheckoutRequestHeading = "## 作業用クローン要求"
)

// MemoryAppends contains optional entries proposed by a work turn. The bot
// chooses the actual channel path from the authenticated Slack event.
type MemoryAppends struct {
	Global  string
	Channel string
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

// SplitCheckoutRequest removes every checkout request heading outside code
// blocks and reports whether there was one.
func SplitCheckoutRequest(text string) (rest string, requested bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if prose[i] && strings.TrimSpace(line) == CheckoutRequestHeading {
			requested = true
			continue
		}
		kept = append(kept, line)
	}
	if !requested {
		return text, false
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), true
}

// SplitAttachments removes the attachment section from a work response. The
// section is its heading followed by bullet lines, one file path each. It
// ends at the first other line, so text or memory sections after it stay in
// place. It must occur at most once. Malformed output yields no attachments
// and reports valid as false, so the bot can tell the user instead of
// guessing which files were meant.
func SplitAttachments(text string) (rest string, paths []string, valid bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	var starts []int
	for i, line := range lines {
		if prose[i] && strings.TrimSpace(line) == attachmentsHeading {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return text, nil, true
	}
	start := starts[0]
	if len(starts) > 1 {
		// Which list is meant is unclear, so none of it is shown or sent.
		return strings.TrimSpace(strings.Join(lines[:start], "\n")), nil, false
	}

	valid = true
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "- ")
		if !prose[i] || !ok {
			// A section with no bullets before other text is malformed; a
			// heading right after it only means the list is empty.
			if len(paths) == 0 && valid && !(prose[i] && strings.HasPrefix(trimmed, "#")) {
				valid = false
			}
			end = i
			break
		}
		item = strings.TrimSpace(item)
		if unquoted, ok := strings.CutPrefix(item, "`"); ok {
			item, ok = strings.CutSuffix(unquoted, "`")
			if !ok {
				valid = false
			}
		}
		if item == "" {
			valid = false
		}
		paths = append(paths, item)
	}
	kept := append(append([]string(nil), lines[:start]...), lines[end:]...)
	rest = strings.TrimSpace(strings.Join(kept, "\n"))
	if !valid {
		return rest, nil, false
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

// FollowUpRequest is the work a turn schedules for itself. When is as the
// model wrote it; the bot decides whether it is a valid time.
type FollowUpRequest struct {
	When string
	Task string
}

// SplitFollowUp removes the follow-up section from a work response. The
// section is its heading followed by the bullets "- いつ: ..." and
// "- やること: ...", each once, and ends at the first other line. It returns
// the request, or nil when there is no section. invalid reports a section
// that appears more than once or has incomplete bullets; it yields no
// request.
func SplitFollowUp(text string) (rest string, request *FollowUpRequest, invalid bool) {
	rest, fields, present, invalid := splitFieldSection(text, followUpHeading)
	if !present {
		return text, nil, false
	}
	if invalid || fields[followUpWhenKey] == "" || fields[followUpTaskKey] == "" {
		return rest, nil, true
	}
	return rest, &FollowUpRequest{When: fields[followUpWhenKey], Task: fields[followUpTaskKey]}, false
}

// splitFieldSection removes the section under heading whose lines are
// "- いつ: ..." and "- やること: ..." bullets, and ends at the first other
// line. present reports the heading; invalid reports a repeated heading,
// field, or an unreadable bullet.
func splitFieldSection(text, heading string) (rest string, fields map[string]string, present, invalid bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	start, count := -1, 0
	for i, line := range lines {
		if prose[i] && strings.TrimSpace(line) == heading {
			if count == 0 {
				start = i
			}
			count++
		}
	}
	if count == 0 {
		return text, nil, false, false
	}
	if count > 1 {
		return strings.TrimSpace(strings.Join(lines[:start], "\n")), nil, true, true
	}
	fields = make(map[string]string)
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "- ")
		if !prose[i] || !ok {
			end = i
			break
		}
		key, value, ok := cutFollowUpField(item)
		if _, seen := fields[key]; !ok || seen {
			invalid = true
			continue
		}
		fields[key] = value
	}
	kept := append(append([]string(nil), lines[:start]...), lines[end:]...)
	return strings.TrimSpace(strings.Join(kept, "\n")), fields, true, invalid
}

// cutFollowUpField splits "key: value", accepting a full-width colon too.
func cutFollowUpField(item string) (key, value string, ok bool) {
	for _, separator := range []string{":", "："} {
		if key, value, ok = strings.Cut(item, separator); ok {
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), "`")
			if key == followUpWhenKey || key == followUpTaskKey {
				return key, strings.TrimSpace(value), value != ""
			}
		}
	}
	return "", "", false
}

// SplitQuestions removes the section where a turn asks the requester
// something it cannot continue without. The section is its heading followed
// by bullet lines, one question each, and ends at the first other line.
// waiting reports that the heading was present, even with no bullets, so a
// turn that stopped to ask is never shown as finished. Only the first
// section counts; later copies of the heading are removed with their bullets.
func SplitQuestions(text string) (rest string, questions []string, waiting bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if !prose[i] || strings.TrimSpace(lines[i]) != questionsHeading {
			kept = append(kept, lines[i])
			continue
		}
		first := !waiting
		waiting = true
		for i+1 < len(lines) {
			trimmed := strings.TrimSpace(lines[i+1])
			if trimmed == "" {
				i++
				continue
			}
			item, ok := strings.CutPrefix(trimmed, "- ")
			if !prose[i+1] || !ok {
				break
			}
			if item = strings.TrimSpace(item); item != "" && first {
				questions = append(questions, item)
			}
			i++
		}
	}
	if !waiting {
		return text, nil, false
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), questions, true
}

// SplitPullWatches removes the section that asks the bot to watch pull
// requests. The section is its heading followed by bullet lines, one pull
// request URL each, and ends at the first other line. Every copy of the
// heading is removed with its bullets; the URLs of all of them are returned
// once each, and the bot decides which are pull requests it can watch.
func SplitPullWatches(text string) (rest string, urls []string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	kept := make([]string, 0, len(lines))
	found := false
	seen := make(map[string]bool)
	for i := 0; i < len(lines); i++ {
		if !prose[i] || strings.TrimSpace(lines[i]) != pullWatchHeading {
			kept = append(kept, lines[i])
			continue
		}
		found = true
		for i+1 < len(lines) {
			trimmed := strings.TrimSpace(lines[i+1])
			if trimmed == "" {
				i++
				continue
			}
			item, ok := strings.CutPrefix(trimmed, "- ")
			if !prose[i+1] || !ok {
				break
			}
			item = strings.Trim(strings.TrimSpace(item), "`<>")
			if item != "" && !seen[item] {
				seen[item] = true
				urls = append(urls, item)
			}
			i++
		}
	}
	if !found {
		return text, nil
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), urls
}
