package slackbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

// threadWorkspaces gives every thread the same private directory, so a test
// can look at what a turn saved.
type threadWorkspaces struct{ dir string }

func (w threadWorkspaces) ThreadDir(string) (string, error) { return w.dir, nil }

func (w threadWorkspaces) Acquire(context.Context, string, bool) (*workspace.Lease, error) {
	return workspace.NewLease(w.dir, nil, nil, func() {}), nil
}

func (w threadWorkspaces) OtherThreadPaths(string) ([]string, error) { return nil, nil }

func newFilesTestBot(t *testing.T, api *fakeSlack, runner *fakeRunner) (*Bot, string) {
	t.Helper()
	dir := t.TempDir()
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
	bot.config.Workspaces = threadWorkspaces{dir: dir}
	return bot, dir
}

func doneRunner() *fakeRunner {
	return &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{Completed: true, Messages: []string{"Done."}}}}}
}

func TestMentionImageIsSavedForTheTurn(t *testing.T) {
	api := &fakeSlack{
		message:   ThreadMessage{AuthorID: "U1", Text: "<@UBOT> do it", Files: []SlackFile{{Name: "pull down.png", Mimetype: "image/png", Size: 4, URL: "https://files/1"}}},
		downloads: map[string]string{"https://files/1": "\x89PNG"},
	}
	runner := doneRunner()
	bot, dir := newFilesTestBot(t, api, runner)

	bot.HandleMention(context.Background(), mention())

	path := filepath.Join(dir, sharedFilesDir, "100-1-pull_down.png")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "\x89PNG" {
		t.Fatalf("saved file = %q, %v; want the image", content, err)
	}
	if len(runner.prompts) != 1 {
		t.Fatalf("runner prompts = %d, want 1", len(runner.prompts))
	}
	for _, want := range []string{sharedFilesHeading, "- " + path + "（image/png、100.1 の発言）"} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("prompt does not contain %q:\n%s", want, runner.prompts[0])
		}
	}
	if strings.Contains(runner.prompts[0], sharedFilesMissing) {
		t.Error("prompt says a file is missing although it was saved")
	}
}

func TestMentionWithoutFilesAddsNoNote(t *testing.T) {
	runner := doneRunner()
	bot, dir := newFilesTestBot(t, &fakeSlack{}, runner)

	bot.HandleMention(context.Background(), mention())

	if strings.Contains(runner.prompts[0], sharedFilesHeading) {
		t.Error("prompt lists files for a mention without files")
	}
	if _, err := os.Stat(filepath.Join(dir, sharedFilesDir)); !os.IsNotExist(err) {
		t.Errorf("files directory exists (%v), want none", err)
	}
}

func TestSignInPageIsReportedAsMissingScope(t *testing.T) {
	api := &fakeSlack{
		message:   ThreadMessage{Files: []SlackFile{{Name: "receipt.jpg", Mimetype: "image/jpeg", URL: "https://files/2"}}},
		downloads: map[string]string{"https://files/2": "\n<!DOCTYPE html><html>sign in</html>"},
	}
	runner := doneRunner()
	bot, dir := newFilesTestBot(t, api, runner)

	bot.HandleMention(context.Background(), mention())

	for _, want := range []string{"receipt.jpg（100.1 の発言）: Slack から取得できませんでした（ebi-x の Slack アプリに files:read 権限が必要です）", sharedFilesMissing} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("prompt does not contain %q:\n%s", want, runner.prompts[0])
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, sharedFilesDir))
	if len(entries) != 0 {
		t.Errorf("files directory has %d entries, want the page discarded", len(entries))
	}
}

func TestFirstTurnSavesEarlierThreadFiles(t *testing.T) {
	api := &fakeSlack{
		threadMessages: []ThreadMessage{
			{AuthorID: "U2", Timestamp: "100.1", Files: []SlackFile{{Name: "screen.png", Mimetype: "image/png", URL: "https://files/3"}}},
			{AuthorID: "U1", Timestamp: "300.3", Text: "<@UBOT> 画像を見て"},
		},
		downloads: map[string]string{"https://files/3": "img"},
	}
	runner := doneRunner()
	bot, dir := newFilesTestBot(t, api, runner)
	event := mention()
	event.TimeStamp = "300.3"
	event.ThreadTimeStamp = "100.1"
	event.Text = "<@UBOT> 画像を見て"

	bot.HandleMention(context.Background(), event)

	if api.messageCalls != 0 {
		t.Errorf("message reads = %d, want the current message taken from the thread", api.messageCalls)
	}
	got := runner.prompts[0]
	for _, want := range []string{"[100.1 / U2]\n（添付: screen.png）", filepath.Join(dir, sharedFilesDir, "100-1-screen.png")} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, got)
		}
	}
}

func TestSharedWorkspaceDoesNotSaveFiles(t *testing.T) {
	api := &fakeSlack{
		message:   ThreadMessage{Files: []SlackFile{{Name: "a.png", Mimetype: "image/png", URL: "https://files/4"}}},
		downloads: map[string]string{"https://files/4": "img"},
	}
	runner := doneRunner()
	newTestBot(t, &fakeStore{claim: true}, api, runner).HandleMention(context.Background(), mention())

	if !strings.Contains(runner.prompts[0], "a.png（100.1 の発言）: 作業領域がスレッド専用でないため取得していません") {
		t.Errorf("prompt = %s, want the file listed as not saved", runner.prompts[0])
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":      "passwd",
		"レシート 1.jpg":            "レシート_1.jpg",
		"..hidden":              "hidden",
		"a\\b/c<d>.png":         "c_d_.png",
		"":                      "file",
		strings.Repeat("x", 99): strings.Repeat("x", 80),
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
