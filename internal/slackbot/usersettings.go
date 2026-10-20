package slackbot

import (
	"fmt"
	"log"

	"github.com/n-seiji/ebi-x/internal/usersettings"
)

const (
	userSettingsSavedNotice   = "⚙️ あなたの個人設定を更新しました（%d件）。内容は ebi-x のアプリのホームで確認できます。このメッセージはあなたにだけ表示されています。"
	userSettingsClearedNotice = "⚙️ あなたの個人設定をすべて消しました。このメッセージはあなたにだけ表示されています。"
	userSettingsFailedNotice  = "⚠️ 個人設定を保存できませんでした。もう一度お試しください。このメッセージはあなたにだけ表示されています。"
)

// readUserSettings returns the speaker's own settings for the prompt. A
// request nobody wrote, such as a follow-up, has no speaker's settings.
func (b *Bot) readUserSettings(userID string) []string {
	if b.config.MemoryDir == "" || !usersettings.ValidUserID(userID) {
		return nil
	}
	b.memoryMu.RLock()
	settings, err := usersettings.Read(b.config.MemoryDir, userID)
	b.memoryMu.RUnlock()
	if err != nil {
		log.Printf("slackbot: read user settings %q: %v", userID, err)
	}
	return settings
}

// saveUserSettings replaces the requester's settings with the ones a work
// turn wrote and returns the notice for the requester. Only a request a
// person wrote changes settings, and only that person's, so a follow-up, a
// pull request event, or a scheduled run cannot change anyone's settings.
func (b *Bot) saveUserSettings(eventKey string, trigger processingTrigger, settings []string) string {
	if trigger.source != mentionTrigger && trigger.source != messageTrigger {
		log.Printf("slackbot: ignore user settings from a request nobody wrote %q", eventKey)
		return ""
	}
	if b.config.MemoryDir == "" || !usersettings.ValidUserID(trigger.authorID) {
		return ""
	}
	b.memoryMu.Lock()
	written, err := usersettings.Write(b.config.MemoryDir, trigger.authorID, settings)
	b.memoryMu.Unlock()
	if err != nil {
		log.Printf("slackbot: write user settings %q: %v", eventKey, err)
		return userSettingsFailedNotice
	}
	if len(written) == 0 {
		return userSettingsClearedNotice
	}
	return fmt.Sprintf(userSettingsSavedNotice, len(written))
}
