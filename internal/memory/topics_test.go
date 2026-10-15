package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTopicUpdates(t *testing.T) {
	updates, rest, err := ParseTopicUpdates(strings.Join([]string{
		"- 話題に分けない学び",
		"```",
		"### transaction-lookup",
		"説明: 取引の問い合わせを調べるとき",
		"- 短縮IDでもまず調べる",
		"",
		"### bigquery",
		"書き換え:",
		"### 2026-10-01",
		"- 読み取り専用で照合する",
		"```",
	}, "\n"))
	if err != nil {
		t.Fatalf("ParseTopicUpdates() error = %v", err)
	}
	if rest != "- 話題に分けない学び" {
		t.Errorf("rest = %q", rest)
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %#v, want 2", updates)
	}
	if got := updates[0]; got.Slug != "transaction-lookup" || got.Description != "取引の問い合わせを調べるとき" || got.Append != "- 短縮IDでもまず調べる" || got.IsRewrite {
		t.Errorf("updates[0] = %#v", got)
	}
	if got := updates[1]; got.Slug != "bigquery" || !got.IsRewrite || got.Rewrite != "### 2026-10-01\n- 読み取り専用で照合する" {
		t.Errorf("updates[1] = %#v", got)
	}
}

func TestParseTopicUpdatesRejectsBadBlocks(t *testing.T) {
	for _, text := range []string{
		"### ../etc\n- x",
		"### Upper\n- x",
		"### a\n- x\n### a\n- y",
		"### empty",
	} {
		if _, _, err := ParseTopicUpdates(text); err == nil {
			t.Errorf("ParseTopicUpdates(%q) succeeded, want error", text)
		}
	}
}

func TestApplyTopicUpdateCreatesAppendsAndRewrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := ApplyTopicUpdate(dir, TopicUpdate{Slug: "lookup", Append: "- x"}, testNow); err == nil {
		t.Fatal("new topic without description succeeded, want error")
	}
	if _, err := ApplyTopicUpdate(dir, TopicUpdate{Slug: "lookup", Description: "取引を調べるとき", Append: "- 一つ目"}, testNow); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ApplyTopicUpdate(dir, TopicUpdate{Slug: "lookup", Append: "- 二つ目"}, testNow); err != nil {
		t.Fatalf("append: %v", err)
	}
	topics, err := LoadTopics(dir)
	if err != nil || len(topics) != 1 {
		t.Fatalf("LoadTopics() = %#v, %v", topics, err)
	}
	if want := "### 2026-10-15\n- 一つ目\n\n### 2026-10-15\n- 二つ目"; topics[0].Description != "取引を調べるとき" || topics[0].Body != want {
		t.Fatalf("topic = %#v, want body %q", topics[0], want)
	}
	if _, err := ApplyTopicUpdate(dir, TopicUpdate{Slug: "lookup", IsRewrite: true, Rewrite: "- まとめた学び"}, testNow); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	topics, _ = LoadTopics(dir)
	if topics[0].Body != "- まとめた学び" || topics[0].Description != "取引を調べるとき" {
		t.Fatalf("rewritten topic = %#v", topics[0])
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "topics-backup", "lookup.*.md"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one", backups)
	}
	if data, _ := os.ReadFile(backups[0]); !strings.Contains(string(data), "二つ目") {
		t.Fatalf("backup = %q", data)
	}
	if _, err := ApplyTopicUpdate(dir, TopicUpdate{Slug: "lookup", IsRewrite: true}, testNow); err == nil {
		t.Fatal("empty rewrite succeeded, want error")
	}
}

func TestApplyScopedWritesTopicsAndLegacy(t *testing.T) {
	root := t.TempDir()
	topics, appended, err := ApplyScoped(root, ScopeChannel, "C1", "- 古い形式の学び\n### lookup\n説明: 調べるとき\n- 新しい学び", testNow)
	if err != nil || !appended || len(topics) != 1 || topics[0] != "lookup" {
		t.Fatalf("ApplyScoped() = %v, %v, %v", topics, appended, err)
	}
	if _, err := os.Stat(filepath.Join(root, "channels", "C1", "topics", "lookup.md")); err != nil {
		t.Fatalf("topic file: %v", err)
	}
	got, err := ReadContext(root, "C1", "")
	if err != nil {
		t.Fatalf("ReadContext() error = %v", err)
	}
	if !strings.Contains(got.Channel, "- lookup: 調べるとき") || !strings.Contains(got.Channel, "古い形式の学び") {
		t.Fatalf("channel memory = %q", got.Channel)
	}
}

func TestRenderScopeShowsIndexAndRelatedTopicsOnly(t *testing.T) {
	topics := []Topic{
		{Slug: "transaction-lookup", Description: "取引の問い合わせを調べるとき", Body: "- 短縮IDでもまず前方一致で調べる"},
		{Slug: "deploy", Description: "デプロイ手順", Body: "- 本番へは金曜に出さない"},
	}
	got := renderScope(topics, "", "取引 327fe9d2 が承認されない", maxPromptBytes)
	for _, want := range []string{"- transaction-lookup: 取引の問い合わせを調べるとき", "- deploy: デプロイ手順", "### 話題: transaction-lookup\n- 短縮IDでも"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderScope() missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "金曜に出さない") {
		t.Errorf("renderScope() should leave out the unrelated topic's body:\n%s", got)
	}
}
