package memory

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Topic files split a scope's memory by subject, as Claude Code keeps one
// memory file per subject with a description used to decide when to read it.
// Every prompt lists each topic with its description, and carries the full
// text only of the topics related to the request, so memory can keep growing
// without crowding the prompt. A turn adds to a topic or rewrites it as a
// whole, which merges duplicates and drops superseded facts in place.
const (
	topicsDirName  = "topics"
	backupsDirName = "topics-backup"
	// MaxTopicBytes caps one topic file's body. Prompts ask for a rewrite of
	// topics past TopicRewriteBytes, before this cap truncates them.
	MaxTopicBytes = 4 << 10
	// TopicRewriteBytes is the size past which a turn that updates a topic
	// should rewrite it shorter.
	TopicRewriteBytes = 3 << 10
	// maxDescriptionBytes caps a topic's one-line description.
	maxDescriptionBytes = 200
	// maxIndexBytes caps the topic list in a prompt.
	maxIndexBytes = 2 << 10
)

var topicSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidTopicSlug reports whether slug can name a topic file.
func ValidTopicSlug(slug string) bool {
	return topicSlug.MatchString(slug)
}

// Topic is one subject's memory file.
type Topic struct {
	Slug        string
	Description string
	Body        string
}

// TopicUpdate is a turn's change to one topic: lines to add under today's
// date, or a whole new body. Description sets or replaces the topic's
// description and is required for a new topic.
type TopicUpdate struct {
	Slug        string
	Description string
	Append      string
	Rewrite     string
	IsRewrite   bool
}

// ParseTopicUpdates reads the topic blocks of a memory section: a "### slug"
// line, an optional "説明: ..." line, then either lines to append or a
// "書き換え:" line followed by the topic's whole new body. Text before the
// first block is returned as rest, for the scope's untopiced MEMORY.md.
// A malformed block, such as an invalid slug or a repeated topic, makes the
// whole section invalid.
func ParseTopicUpdates(text string) (updates []TopicUpdate, rest string, err error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var before []string
	var current *TopicUpdate
	var body []string
	seen := map[string]bool{}
	flush := func() error {
		if current == nil {
			return nil
		}
		content := strings.TrimSpace(strings.Join(body, "\n"))
		if current.IsRewrite {
			current.Rewrite = content
		} else {
			current.Append = content
		}
		if content == "" && current.Description == "" {
			return fmt.Errorf("topic %q has no content", current.Slug)
		}
		updates = append(updates, *current)
		current, body = nil, nil
		return nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			// Turns sometimes fence their blocks like the prompt's example.
			continue
		}
		if slug, ok := strings.CutPrefix(trimmed, "### "); ok && !dateHeading.MatchString(trimmed) {
			if err := flush(); err != nil {
				return nil, "", err
			}
			slug = strings.TrimSpace(slug)
			if !ValidTopicSlug(slug) {
				return nil, "", fmt.Errorf("invalid topic name %q", slug)
			}
			if seen[slug] {
				return nil, "", fmt.Errorf("topic %q appears twice", slug)
			}
			seen[slug] = true
			current = &TopicUpdate{Slug: slug}
			continue
		}
		if current == nil {
			before = append(before, line)
			continue
		}
		if len(body) == 0 && !current.IsRewrite {
			if description, ok := strings.CutPrefix(trimmed, "説明:"); ok && current.Description == "" {
				current.Description = strings.TrimSpace(description)
				continue
			}
			if trimmed == "書き換え:" {
				current.IsRewrite = true
				continue
			}
		}
		body = append(body, line)
	}
	if err := flush(); err != nil {
		return nil, "", err
	}
	return updates, strings.TrimSpace(strings.Join(before, "\n")), nil
}

// LoadTopics reads the scope directory's topic files, sorted by slug. A
// missing directory means no topics; unreadable files are reported and
// skipped.
func LoadTopics(dir string) ([]Topic, error) {
	entries, err := os.ReadDir(filepath.Join(dir, topicsDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var topics []Topic
	var errs []error
	for _, item := range entries {
		slug, ok := strings.CutSuffix(item.Name(), ".md")
		if !ok || !item.Type().IsRegular() || !ValidTopicSlug(slug) {
			continue
		}
		topic, err := readTopic(dir, slug)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		topics = append(topics, topic)
	}
	return topics, errors.Join(errs...)
}

func topicPath(dir, slug string) string {
	return filepath.Join(dir, topicsDirName, slug+".md")
}

func readTopic(dir, slug string) (Topic, error) {
	data, err := os.ReadFile(topicPath(dir, slug))
	if err != nil {
		return Topic{}, err
	}
	topic := Topic{Slug: slug}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if front, body, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n"); ok && strings.HasPrefix(text, "---\n") {
		for _, line := range strings.Split(front, "\n") {
			if value, ok := strings.CutPrefix(line, "description:"); ok {
				topic.Description = strings.TrimSpace(value)
			}
		}
		text = body
	}
	topic.Body = strings.TrimSpace(text)
	return topic, nil
}

// ApplyTopicUpdate writes one update to the scope directory's topic file and
// returns the slug it changed, or "" when there was nothing to write. A
// rewrite keeps the previous file under topics-backup so it can be restored
// by hand. Callers must serialize ApplyTopicUpdate with other memory writes.
func ApplyTopicUpdate(dir string, update TopicUpdate, now time.Time) (string, error) {
	if !ValidTopicSlug(update.Slug) {
		return "", fmt.Errorf("invalid topic name %q", update.Slug)
	}
	topic, err := readTopic(dir, update.Slug)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	topic.Slug = update.Slug
	if description := strings.Join(strings.Fields(update.Description), " "); description != "" {
		topic.Description = truncateRunes(description, maxDescriptionBytes)
	}
	if topic.Description == "" {
		return "", fmt.Errorf("new topic %q has no description", update.Slug)
	}
	switch {
	case update.IsRewrite:
		if strings.TrimSpace(update.Rewrite) == "" {
			return "", fmt.Errorf("rewrite of topic %q is empty", update.Slug)
		}
		if exists {
			if err := backupTopic(dir, update.Slug, now); err != nil {
				return "", err
			}
		}
		topic.Body = strings.TrimSpace(update.Rewrite)
	case strings.TrimSpace(update.Append) != "":
		topic.Body = strings.TrimSpace(topic.Body + "\n\n### " + now.Format(time.DateOnly) + "\n" + strings.TrimSpace(update.Append))
	case !exists:
		return "", fmt.Errorf("new topic %q has no content", update.Slug)
	}
	if len(topic.Body) > MaxTopicBytes {
		// Keep the newest text, as prompts did for the old single file.
		topic.Body = "(前半はサイズ上限のため省略)\n" + strings.TrimSpace(truncateTailRunes(topic.Body, MaxTopicBytes))
	}
	content := "---\nname: " + topic.Slug + "\ndescription: " + topic.Description + "\n---\n\n" + topic.Body + "\n"
	if err := writeFileAtomic(topicPath(dir, update.Slug), content); err != nil {
		return "", err
	}
	return update.Slug, nil
}

func backupTopic(dir, slug string, now time.Time) error {
	data, err := os.ReadFile(topicPath(dir, slug))
	if err != nil {
		return err
	}
	backups := filepath.Join(dir, backupsDirName)
	if err := os.MkdirAll(backups, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(backups, slug+"."+now.Format("20060102-150405")+".md"), data, 0o600)
}

func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// renderScope builds what a prompt carries for one scope: the list of every
// topic, the full text of the topics related to query, and the untopiced
// MEMORY.md, all within limit bytes. Topics are ranked like recall ranks
// entries, matching query against each topic's name, description and body.
func renderScope(topics []Topic, legacy, query string, limit int) string {
	legacy = strings.TrimSpace(legacy)
	if len(topics) == 0 {
		return recall(legacy, query, limit)
	}
	var index strings.Builder
	index.WriteString("話題の一覧（name: description）:\n")
	for _, topic := range topics {
		line := fmt.Sprintf("- %s: %s（%.1fKB）\n", topic.Slug, topic.Description, float64(len(topic.Body))/1024)
		if index.Len()+len(line) > maxIndexBytes {
			index.WriteString("- （一覧が長いため省略）\n")
			break
		}
		index.WriteString(line)
	}

	queryTerms := termsOf(query)
	type candidate struct {
		topic Topic
		score float64
	}
	var related []candidate
	for _, topic := range topics {
		terms := termsOf(topic.Slug + " " + topic.Description + " " + topic.Body)
		shared := 0
		for term := range queryTerms {
			if _, ok := terms[term]; ok {
				shared++
			}
		}
		if shared > 0 {
			related = append(related, candidate{topic: topic, score: float64(shared) / math.Sqrt(float64(len(terms)))})
		}
	}
	sort.SliceStable(related, func(a, b int) bool { return related[a].score > related[b].score })

	// Untopiced memory keeps a quarter of the space when there is any, so
	// facts written before topics existed still reach prompts.
	budget := limit - index.Len()
	if legacy != "" {
		budget -= limit / 4
	}
	var bodies strings.Builder
	for _, item := range related {
		block := fmt.Sprintf("\n### 話題: %s\n%s\n", item.topic.Slug, item.topic.Body)
		if bodies.Len()+len(block) > budget {
			continue
		}
		bodies.WriteString(block)
	}
	result := index.String()
	if bodies.Len() > 0 {
		result += "\nこの依頼に関係しそうな話題の全文:" + bodies.String()
	}
	if legacy != "" {
		remaining := limit - len(result) - 64
		if remaining > 0 {
			result += "\n話題に分けていないメモリ:\n" + recall(legacy, query, remaining)
		}
	}
	return strings.TrimSpace(result)
}
