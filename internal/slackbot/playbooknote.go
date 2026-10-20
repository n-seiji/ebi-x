package slackbot

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/playbook"
	"github.com/n-seiji/ebi-x/internal/prompt"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

const (
	playbookNoteInvalidNotice = "⚠️ playbook メモの指定を読み取れなかったため、保存していません。"
	// playbookFoldThreshold is how many notes a playbook gathers before the
	// bot folds them into its body. Notes are meant to bridge edits, not to
	// replace them, so the lesson ends up in the playbook itself, where every
	// later turn follows it.
	playbookFoldThreshold = 3
	// maxFoldSummaryRunes caps the summary of a fold shown in Slack.
	maxFoldSummaryRunes = 300

	playbookFoldedNotice  = "📘 メモが%d件たまったため、playbook「%s」の本文に取り込みました: %s"
	playbookFoldSkipped   = "📘 playbook「%s」のメモ%d件の取り込みを試みましたが、本文は変わりませんでした: %s"
	playbookFoldFailed    = "⚠️ playbook「%s」へのメモの取り込みに失敗しました。メモはそのまま残っています。"
	playbookFoldAdminPost = "📘 playbook「%s」にメモ%d件を取り込みました（%s）: %s"
)

// playbookFold is a fold of a playbook's notes into its body.
type playbookFold struct {
	name    string
	notes   int
	changed bool
	summary string
}

// playbookNotesDir keeps the playbook notes beside the memory, which the
// agent can neither read nor write directly.
func (b *Bot) playbookNotesDir() string {
	return filepath.Join(b.config.MemoryDir, "playbooks")
}

// currentPlaybooks lists the playbooks as they are now, with their notes.
func (b *Bot) currentPlaybooks() ([]playbook.Playbook, error) {
	playbooks := append([]playbook.Playbook(nil), b.playbooks...)
	if b.config.PlaybooksDir != "" {
		var err error
		if playbooks, err = playbook.List(b.config.PlaybooksDir); err != nil {
			return nil, err
		}
	}
	if b.config.MemoryDir != "" {
		b.memoryMu.RLock()
		err := playbook.AttachNotes(b.playbookNotesDir(), playbooks)
		b.memoryMu.RUnlock()
		if err != nil {
			log.Printf("slackbot: read playbook notes: %v", err)
		}
	}
	return playbooks, nil
}

// savePlaybookNote adds the note a work turn wrote to the playbook it names
// and returns what to tell the thread. Playbooks reach every channel, so only
// channels that may change them may add notes, as with global memory.
//
// fold is the playbook whose notes reached playbookFoldThreshold, which the
// caller folds into its body.
func (b *Bot) savePlaybookNote(eventKey string, note *codex.PlaybookNote, invalid, sharedWritable bool) (notice string, fold *playbook.Playbook) {
	if invalid {
		log.Printf("slackbot: ignore malformed playbook note output %q", eventKey)
		return playbookNoteInvalidNotice, nil
	}
	if note == nil {
		return "", nil
	}
	if !sharedWritable || b.config.MemoryDir == "" {
		log.Printf("slackbot: ignore playbook note from channel without shared writes %q", eventKey)
		return "", nil
	}
	playbooks, err := b.currentPlaybooks()
	if err != nil {
		log.Printf("slackbot: list playbooks for note %q: %v", eventKey, err)
		return fmt.Sprintf("⚠️ playbook「%s」のメモを保存できませんでした。", note.Target), nil
	}
	target, ok := playbook.Find(playbooks, note.Target)
	if !ok {
		return fmt.Sprintf("⚠️ playbook「%s」が見つからないため、メモを保存していません。", note.Target), nil
	}
	b.memoryMu.Lock()
	written, err := playbook.AppendNote(b.playbookNotesDir(), target, note.Body, b.now())
	b.memoryMu.Unlock()
	if err != nil {
		log.Printf("slackbot: append playbook note %q: %v", eventKey, err)
		return fmt.Sprintf("⚠️ playbook「%s」のメモを保存できませんでした。", target.Name), nil
	}
	if written == "" {
		return "", nil
	}
	notice = fmt.Sprintf("📝 playbook「%s」のメモを更新しました: %s", target.Name, written)
	if current, ok := b.findPlaybook(target.Name); ok && noteCount(current) >= playbookFoldThreshold {
		fold = &current
	}
	return notice, fold
}

// findPlaybook reloads the playbook named name with the notes it shows now.
func (b *Bot) findPlaybook(name string) (playbook.Playbook, bool) {
	playbooks, err := b.currentPlaybooks()
	if err != nil {
		log.Printf("slackbot: reload playbook %q: %v", name, err)
		return playbook.Playbook{}, false
	}
	return playbook.Find(playbooks, name)
}

// noteCount reports how many notes item shows, that is, the notes written
// since its body last changed.
func noteCount(item playbook.Playbook) int {
	if item.Notes == "" {
		return 0
	}
	return len(strings.Split(item.Notes, "\n"))
}

// foldPlaybookNotes continues the turn's session to fold the notes of item
// into its body without asking, as the operator chose, and reports what
// happened. Editing the body retires the notes it folds in.
func (b *Bot) foldPlaybookNotes(ctx context.Context, sessionID string, lease *workspace.Lease, otherThreads []string, item playbook.Playbook, onActivity func(codex.Activity)) *playbookFold {
	fold := &playbookFold{name: item.Name, notes: noteCount(item)}
	text, err := b.runWorkTurn(ctx, sessionID, lease, true, otherThreads, prompt.BuildPlaybookFoldPrompt(item), func(string) error { return nil }, onActivity)
	if err != nil {
		log.Printf("slackbot: fold notes of playbook %q: %v", item.Name, err)
		return fold
	}
	fold.summary = clipText(strings.Join(strings.Fields(codex.SanitizeSlackOutput(text)), " "), maxFoldSummaryRunes)
	if after, ok := b.findPlaybook(item.Name); ok && !after.ModTime.Equal(item.ModTime) {
		fold.changed = true
	}
	return fold
}

// notice tells the thread what became of the fold.
func (f *playbookFold) notice() string {
	switch {
	case f.changed:
		return fmt.Sprintf(playbookFoldedNotice, f.notes, f.name, f.summary)
	case f.summary != "":
		return fmt.Sprintf(playbookFoldSkipped, f.name, f.notes, f.summary)
	default:
		return fmt.Sprintf(playbookFoldFailed, f.name)
	}
}

// recordPlaybookFold posts a changed playbook to the approval channel, which
// keeps the history of what changed every channel's playbooks.
func (b *Bot) recordPlaybookFold(ctx context.Context, fold *playbookFold, channel, threadTS string) {
	if fold == nil || !fold.changed || b.config.ApprovalChannelID == "" {
		return
	}
	link := b.threadLink(ctx, make(map[string]string), channel, threadTS)
	if err := b.post(ctx, b.config.ApprovalChannelID, "", fmt.Sprintf(playbookFoldAdminPost, fold.name, fold.notes, link, fold.summary)); err != nil {
		log.Printf("slackbot: record playbook fold %q: %v", fold.name, err)
	}
}
