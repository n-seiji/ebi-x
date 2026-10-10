package slackbot

import (
	"fmt"
	"log"
	"path/filepath"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/playbook"
)

const playbookNoteInvalidNotice = "⚠️ playbook メモの指定を読み取れなかったため、保存していません。"

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
func (b *Bot) savePlaybookNote(eventKey string, note *codex.PlaybookNote, invalid, sharedWritable bool) string {
	if invalid {
		log.Printf("slackbot: ignore malformed playbook note output %q", eventKey)
		return playbookNoteInvalidNotice
	}
	if note == nil {
		return ""
	}
	if !sharedWritable || b.config.MemoryDir == "" {
		log.Printf("slackbot: ignore playbook note from channel without shared writes %q", eventKey)
		return ""
	}
	playbooks, err := b.currentPlaybooks()
	if err != nil {
		log.Printf("slackbot: list playbooks for note %q: %v", eventKey, err)
		return fmt.Sprintf("⚠️ playbook「%s」のメモを保存できませんでした。", note.Target)
	}
	target, ok := playbook.Find(playbooks, note.Target)
	if !ok {
		return fmt.Sprintf("⚠️ playbook「%s」が見つからないため、メモを保存していません。", note.Target)
	}
	b.memoryMu.Lock()
	written, err := playbook.AppendNote(b.playbookNotesDir(), target, note.Body, b.now())
	b.memoryMu.Unlock()
	if err != nil {
		log.Printf("slackbot: append playbook note %q: %v", eventKey, err)
		return fmt.Sprintf("⚠️ playbook「%s」のメモを保存できませんでした。", target.Name)
	}
	if written == "" {
		return ""
	}
	return fmt.Sprintf("📝 playbook「%s」のメモを更新しました: %s", target.Name, written)
}
