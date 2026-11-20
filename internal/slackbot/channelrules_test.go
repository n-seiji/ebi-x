package slackbot

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/n-seiji/ebi-x/internal/codex"
)

func writeChannelRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "channel-rules", "channels.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChannelRuleLimitsTheTurn(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{codex.CheckoutRequestHeading}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"続きです。"}}},
	}}
	workspaces := &fakeWorkspaces{lazy: true}
	bot := newLazyCheckoutBot(t, api, runner, workspaces)
	path := writeChannelRules(t, `
channels:
  C1:
    # 社外の方がいるチャンネル
    system_prompt: |
      仕様の言葉で答えてください。
    read_only: true
    github: false
    repositories: [docs]
    mcp_servers: [manual]
  C2:
    system_prompt: ほかのチャンネル
`)
	bot.config.ChannelRulesFile = path

	bot.HandleMention(context.Background(), mention())

	if !reflect.DeepEqual(runner.sandboxes, []string{"read-only"}) {
		t.Fatalf("sandboxes = %v, want one read-only turn", runner.sandboxes)
	}
	access := runner.access[0]
	if !reflect.DeepEqual(access.MCPServers, []string{"manual"}) || !access.NoGitHub || access.WithoutSlack {
		t.Fatalf("access = %+v, want only the manual server and no GitHub", access)
	}
	if !strings.Contains(access.Instructions, "仕様の言葉で答えてください。") || !strings.Contains(access.Instructions, "push や PR の作成はできません") || strings.Contains(access.Instructions, "ほかのチャンネル") {
		t.Fatalf("instructions = %q, want only C1's system prompt and limits", access.Instructions)
	}
	if strings.Contains(runner.prompts[0], "- /src/app") || !reflect.DeepEqual(workspaces.creates, []bool{false}) {
		t.Fatalf("checkouts offered or created: prompt %q, creates %v", runner.prompts[0], workspaces.creates)
	}
	if !slices.Contains(runner.denied[0], "/src/app") || !slices.Contains(runner.denied[0], filepath.Dir(path)) {
		t.Fatalf("denied = %v, want the repository and the rules hidden", runner.denied[0])
	}
}

func TestAllowedRepositoryStaysReadable(t *testing.T) {
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"回答です。"}}},
	}}
	bot := newLazyCheckoutBot(t, &fakeSlack{}, runner, &fakeWorkspaces{lazy: true})
	bot.config.ChannelRulesFile = writeChannelRules(t, "channels:\n  C1:\n    repositories: [app]\n")

	bot.HandleMention(context.Background(), mention())

	if !strings.Contains(runner.prompts[0], "- /src/app") || slices.Contains(runner.denied[0], "/src/app") {
		t.Fatalf("prompt %q, denied %v; want the allowed repository offered", runner.prompts[0], runner.denied[0])
	}
}

func TestChannelRuleAppliesToResumedSessions(t *testing.T) {
	runner := completedRunner()
	store := &fakeStore{claim: true, threadIDs: map[string]string{"v5:C1:100.1": "codex-thread"}}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	bot.config.Workspaces = &fakeWorkspaces{}
	bot.config.ChannelRulesFile = writeChannelRules(t, "channels:\n  C1:\n    system_prompt: 社内の名前は出さないでください。\n")

	bot.HandleMention(context.Background(), mention())

	if runner.threadIDs[0] != "codex-thread" || !strings.Contains(runner.access[0].Instructions, "社内の名前は出さないでください。") {
		t.Fatalf("thread %v, access %+v; want the system prompt on a resumed session", runner.threadIDs, runner.access)
	}
	if !reflect.DeepEqual(runner.sandboxes, []string{"workspace-write"}) || runner.access[0].NoGitHub || runner.access[0].MCPServers != nil {
		t.Fatalf("sandbox, access = %v, %+v; want no limits for a system prompt alone", runner.sandboxes, runner.access)
	}
}

func TestBrokenChannelRulesStopTheWork(t *testing.T) {
	for name, content := range map[string]string{
		"invalid yaml":  "channels: [",
		"unknown field": "channels:\n  C1:\n    readonly: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeSlack{}
			runner := completedRunner()
			bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
			bot.config.Workspaces = &fakeWorkspaces{}
			bot.config.ChannelRulesFile = writeChannelRules(t, content)

			bot.HandleMention(context.Background(), mention())

			if len(runner.prompts) != 0 {
				t.Fatalf("turns = %d, want none while the rules cannot be read", len(runner.prompts))
			}
			if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "作業を開始できなかった") {
				t.Fatalf("posts = %q, want the start failure message", api.postTexts)
			}
		})
	}
}

func TestApprovalChannelEditsChannelRules(t *testing.T) {
	api := &fakeSlack{}
	runner := completedRunner()
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
	bot.config.Workspaces = &fakeWorkspaces{}
	bot.config.ApprovalChannelID = "C1"
	path := writeChannelRules(t, "channels: [")
	bot.config.ChannelRulesFile = path

	bot.HandleMention(context.Background(), mention())

	if len(runner.roots) != 1 || !slices.Contains(runner.roots[0], filepath.Dir(path)) || slices.Contains(runner.denied[0], filepath.Dir(path)) {
		t.Fatalf("roots %v, denied %v; want the rules directory writable", runner.roots, runner.denied)
	}
	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "チャンネルごとの設定ファイルを読めません") {
		t.Fatalf("posts = %q, want the broken file reported", api.postTexts)
	}
}
