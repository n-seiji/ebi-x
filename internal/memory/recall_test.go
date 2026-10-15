package memory

import (
	"fmt"
	"strings"
	"testing"
)

func TestRecallReturnsSmallFileWhole(t *testing.T) {
	text := "### 2026-10-01\n- a\n\n### 2026-10-02\n- b"
	if got := recall(text, "anything", maxPromptBytes); got != text {
		t.Fatalf("recall() = %q, want whole file", got)
	}
}

func TestRecallKeepsRelatedOldEntryAndNewestEntries(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("### 2026-01-01\n- 取引の短縮IDでも、まず前方一致で調べてから完全なIDを聞く\n")
	for i := range 200 {
		fmt.Fprintf(&builder, "\n### 2026-02-%02d\n- 無関係な学び%03d: デプロイ前に lint を回す\n", i%28+1, i)
	}
	text := builder.String()
	got := recall(text, "取引番号 327fe9d2 が承認されない理由を調べて", 2<<10)
	if len(got) > 2<<10+256 {
		t.Fatalf("recall() length = %d, want near %d", len(got), 2<<10)
	}
	if !strings.Contains(got, "短縮IDでも") {
		t.Errorf("recall() lost the related old entry:\n%s", got)
	}
	if !strings.Contains(got, "無関係な学び199") {
		t.Errorf("recall() lost the newest entry:\n%s", got)
	}
	if strings.Contains(got, "無関係な学び000") {
		t.Errorf("recall() kept an old unrelated entry:\n%s", got)
	}
	if !strings.HasPrefix(got, "(全201件のうち") {
		t.Errorf("recall() header = %q", strings.SplitN(got, "\n", 2)[0])
	}
	if strings.Index(got, "短縮IDでも") > strings.Index(got, "無関係な学び199") {
		t.Error("recall() should keep file order")
	}
}

func TestRecallSplitsUndatedParagraphs(t *testing.T) {
	text := "# Memory\n\n- BigQuery は読み取り専用のクエリだけ使える\n\n" + strings.Repeat("- 古い雑多なメモ\n\n", 500) + "- 最新の運用メモ"
	got := recall(text, "BigQuery で集計して", 1<<10)
	if !strings.Contains(got, "BigQuery は読み取り専用") || !strings.Contains(got, "最新の運用メモ") {
		t.Fatalf("recall() = %q", got)
	}
}

func TestTermsOfMixesWordsAndBigrams(t *testing.T) {
	terms := termsOf("Go の go test を実行")
	for _, want := range []string{"go", "test", "実行"} {
		if _, ok := terms[want]; !ok {
			t.Errorf("termsOf() missing %q in %v", want, terms)
		}
	}
	if _, ok := terms["の"]; ok {
		t.Error("termsOf() should not keep single characters")
	}
	if _, ok := termsOf("調べてください")["くだ"]; ok {
		t.Error("termsOf() should drop hiragana-only bigrams")
	}
}
