package codex

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	FollowUpMinDelay = 5 * time.Minute
	FollowUpMaxDelay = 7 * 24 * time.Hour
	MaxFollowUpChain = 10
)

func FollowUpRange() string {
	minimum := strings.TrimSuffix(FollowUpMinDelay.String(), "0s")
	return fmt.Sprintf("%s後から%d日後まで", minimum, int(FollowUpMaxDelay/(24*time.Hour)))
}

func FollowUpDueAt(when string, now time.Time) (time.Time, error) {
	return dueAt(when, now, FollowUpMinDelay, FollowUpMaxDelay)
}

// A reminder only posts a message, so it may be set much further ahead than a
// follow-up.
const (
	ReminderMinDelay = time.Minute
	ReminderMaxDelay = 366 * 24 * time.Hour
)

func ReminderRange() string {
	return fmt.Sprintf("%d分後から%d日後まで", int(ReminderMinDelay/time.Minute), int(ReminderMaxDelay/(24*time.Hour)))
}

func ReminderDueAt(when string, now time.Time) (time.Time, error) {
	return dueAt(when, now, ReminderMinDelay, ReminderMaxDelay)
}

func dueAt(when string, now time.Time, minDelay, maxDelay time.Duration) (time.Time, error) {
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
	if delay := dueAt.Sub(now); delay < minDelay || delay > maxDelay {
		return time.Time{}, fmt.Errorf("delay %v is out of range", delay)
	}
	return dueAt, nil
}

const (
	memoryHeading       = "## メモリ追記"
	globalMemoryHeading = "## 全体メモリ追記"
	// retiredChannelMemoryHeading is still recognized so a turn that writes
	// it has the section stripped from its result, but nothing is saved.
	retiredChannelMemoryHeading = "## チャンネルメモリ追記"
	forbiddenUserMemoryHeading  = "## ユーザーメモリ追記"
	attachmentsHeading          = "## 添付ファイル"
	followUpHeading             = "## フォローアップ"
	followUpWhenKey             = "いつ"
	followUpTaskKey             = "やること"
	reminderHeading             = "## リマインダー"
	reminderTextKey             = "内容"
	ReminderStopWord            = "停止"
	questionsHeading            = "## 回答待ち"
	pullWatchHeading            = "## PR の見守り"
	UserSettingsHeading         = "## 個人設定"
	userSettingsClear           = "なし"
	CheckoutRequestHeading      = "## 作業用クローン要求"
)

type MemoryAppends struct {
	Global string
}

// Truncating at the first occurrence, not just the section, keeps fenced,
// inline, and malformed variants from exposing the private payload after it.
func SanitizeSlackOutput(text string) string {
	index := strings.Index(text, forbiddenUserMemoryHeading)
	if index < 0 {
		return text
	}
	return strings.TrimSpace(text[:index])
}

// Ambiguous output writes no memory rather than guessing which entry was meant.
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
		case retiredChannelMemoryHeading:
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
		if section.scope == 0 {
			appends.Global = entry
		}
	}
	return result, appends, true
}

func SplitMemoryAppend(text string) (rest, entry string) {
	rest, appends, valid := SplitMemoryAppends(text)
	if !valid {
		return text, ""
	}
	return rest, appends.Global
}

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

// Malformed output yields no attachments, so the bot tells the user instead of
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

// Headings quoted in code are not output-contract sections.
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

// When is kept as the model wrote it; the bot decides whether it is a valid
// time.
type FollowUpRequest struct {
	When string
	Task string
}

func SplitFollowUp(text string) (rest string, request *FollowUpRequest, invalid bool) {
	rest, fields, present, invalid := splitFieldSection(text, followUpHeading, followUpWhenKey, followUpTaskKey)
	if !present {
		return text, nil, false
	}
	if invalid || fields[followUpWhenKey] == "" || fields[followUpTaskKey] == "" {
		return rest, nil, true
	}
	return rest, &FollowUpRequest{When: fields[followUpWhenKey], Task: fields[followUpTaskKey]}, false
}

// When is kept as the model wrote it; the bot decides whether it is a valid
// time.
type ReminderRequest struct {
	When string
	Text string
	Stop bool
}

func SplitReminder(text string) (rest string, request *ReminderRequest, invalid bool) {
	rest, fields, present, invalid := splitFieldSection(text, reminderHeading, followUpWhenKey, reminderTextKey)
	if !present {
		return text, nil, false
	}
	if invalid {
		return rest, nil, true
	}
	if fields[followUpWhenKey] == ReminderStopWord {
		return rest, &ReminderRequest{Stop: true}, false
	}
	if fields[followUpWhenKey] == "" || fields[reminderTextKey] == "" {
		return rest, nil, true
	}
	return rest, &ReminderRequest{When: fields[followUpWhenKey], Text: fields[reminderTextKey]}, false
}

func splitFieldSection(text, heading string, keys ...string) (rest string, fields map[string]string, present, invalid bool) {
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
		key, value, ok := cutField(item, keys)
		if _, seen := fields[key]; !ok || seen {
			invalid = true
			continue
		}
		fields[key] = value
	}
	kept := append(append([]string(nil), lines[:start]...), lines[end:]...)
	return strings.TrimSpace(strings.Join(kept, "\n")), fields, true, invalid
}

func cutField(item string, keys []string) (key, value string, ok bool) {
	for _, separator := range []string{":", "："} {
		if key, value, ok = strings.Cut(item, separator); ok {
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), "`")
			if slices.Contains(keys, key) {
				return key, strings.TrimSpace(value), value != ""
			}
		}
	}
	return "", "", false
}

// waiting is true even with no bullets, so a turn that stopped to ask is never
// shown as finished.
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

// Every copy of the heading is removed so none reaches Slack.
func SplitUserSettings(text string) (rest string, settings []string, present, invalid bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	prose := proseLines(lines)
	kept := make([]string, 0, len(lines))
	count := 0
	for i := 0; i < len(lines); i++ {
		if !prose[i] || strings.TrimSpace(lines[i]) != UserSettingsHeading {
			kept = append(kept, lines[i])
			continue
		}
		count++
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
			if item = strings.TrimSpace(item); item != "" && count == 1 {
				settings = append(settings, item)
			}
			i++
		}
	}
	if count == 0 {
		return text, nil, false, false
	}
	rest = strings.TrimSpace(strings.Join(kept, "\n"))
	// A heading without bullets says nothing, so it must not clear the
	// settings.
	if count > 1 || len(settings) == 0 {
		return rest, nil, true, true
	}
	if len(settings) == 1 && settings[0] == userSettingsClear {
		settings = nil
	}
	return rest, settings, true, false
}
