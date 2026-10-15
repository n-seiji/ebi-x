package memory

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// dateHeading matches the heading Append writes above each entry.
var dateHeading = regexp.MustCompile(`^### \d{4}-\d{2}-\d{2}\s*$`)

// entry is one recallable unit of a memory file: a dated append, or a
// paragraph an operator wrote without a date.
type entry struct {
	text  string
	terms map[string]struct{}
}

// splitEntries breaks a memory file into entries in file order. Each dated
// append becomes one entry with its heading; text without a heading is split
// at blank lines so a hand-written MEMORY.md is still recalled piece by piece.
func splitEntries(text string) []entry {
	var entries []entry
	var current []string
	dated := false
	flush := func() {
		body := strings.TrimSpace(strings.Join(current, "\n"))
		if body != "" {
			entries = append(entries, entry{text: body, terms: termsOf(body)})
		}
		current = nil
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case dateHeading.MatchString(line):
			flush()
			dated = true
			current = append(current, line)
		case !dated && strings.TrimSpace(line) == "":
			flush()
		default:
			current = append(current, line)
		}
	}
	flush()
	return entries
}

// termsOf returns the search terms of text: lower-cased words of letters and
// digits, plus character bigrams of runs of other scripts such as Japanese,
// which has no spaces between words. Bigrams keep matching simple and need no
// dictionary or external service.
func termsOf(text string) map[string]struct{} {
	terms := make(map[string]struct{})
	var word []rune
	var run []rune
	flushWord := func() {
		if len(word) >= 2 {
			terms[string(word)] = struct{}{}
		}
		word = word[:0]
	}
	flushRun := func() {
		for i := 0; i+1 < len(run); i++ {
			// Bigrams of hiragana alone are mostly particles and verb
			// endings ("して", "ます"), which match nearly everything.
			if unicode.Is(unicode.Hiragana, run[i]) && unicode.Is(unicode.Hiragana, run[i+1]) {
				continue
			}
			terms[string(run[i:i+2])] = struct{}{}
		}
		run = run[:0]
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushRun()
			word = append(word, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushWord()
			run = append(run, r)
		default:
			flushWord()
			flushRun()
		}
	}
	flushWord()
	flushRun()
	return terms
}

// recall fits a memory file into limit bytes. A file that fits is returned
// whole. Otherwise the entries sharing the most terms with query come first,
// using up to half of limit, and the newest entries fill the rest, so a topic
// brings back what was learned about it however old, while recent learnings
// still reach every request. Chosen entries keep their file order.
func recall(text, query string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	entries := splitEntries(text)
	queryTerms := termsOf(query)
	type candidate struct {
		index int
		score float64
	}
	var related []candidate
	for i, item := range entries {
		shared := 0
		for term := range queryTerms {
			if _, ok := item.terms[term]; ok {
				shared++
			}
		}
		if shared == 0 {
			continue
		}
		// Dividing by the entry's size keeps a long entry that mentions
		// everything from crowding out a short one about the topic.
		related = append(related, candidate{index: i, score: float64(shared) / math.Sqrt(float64(len(item.terms)))})
	}
	sort.SliceStable(related, func(a, b int) bool {
		if related[a].score != related[b].score {
			return related[a].score > related[b].score
		}
		return related[a].index > related[b].index
	})

	chosen := make(map[int]bool)
	used := 0
	take := func(i, budget int) {
		if chosen[i] {
			return
		}
		size := len(entries[i].text) + 2
		if used+size > budget {
			return
		}
		chosen[i] = true
		used += size
	}
	for _, item := range related {
		take(item.index, limit/2)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		take(i, limit)
	}

	var parts []string
	for i, item := range entries {
		if chosen[i] {
			parts = append(parts, item.text)
		}
	}
	if len(parts) == 0 {
		// Only entries larger than the limit, such as one long hand-written
		// paragraph: keep its end, which is the newest text.
		return "(前半はサイズ上限のため省略)\n" + strings.TrimSpace(truncateTailRunes(text, limit))
	}
	header := fmt.Sprintf("(全%d件のうち、この依頼に関係しそうな項目と新しい項目の%d件だけを載せています)", len(entries), len(parts))
	return header + "\n" + strings.Join(parts, "\n\n")
}
