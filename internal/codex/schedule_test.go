package codex

import (
	"testing"
	"time"
)

func TestParseScheduleSpec(t *testing.T) {
	for input, want := range map[string]string{
		"毎日 09:00":       "毎日 09:00",
		"平日 9:30":        "平日 09:30",
		"週末 10：00":       "週末 10:00",
		"毎週月水金 08:15":    "毎週月水金曜 08:15",
		"毎週月・木曜日 9:00":   "毎週月木曜 09:00",
		"毎週日曜 23:59":     "毎週日曜 23:59",
		"  毎週火曜   7:05 ": "毎週火曜 07:05",
	} {
		spec, err := ParseScheduleSpec(input)
		if err != nil {
			t.Errorf("ParseScheduleSpec(%q) error = %v", input, err)
			continue
		}
		if got := spec.String(); got != want {
			t.Errorf("ParseScheduleSpec(%q) = %q, want %q", input, got, want)
		}
		if again, err := ParseScheduleSpec(spec.String()); err != nil || again != spec {
			t.Errorf("String() of %q does not read back: %v", input, err)
		}
	}
	for _, input := range []string{"", "毎日", "毎時 00", "30m", "毎日 24:00", "毎日 9:5", "毎週 9:00", "毎週月X 9:00", "毎日 9:00 から"} {
		if _, err := ParseScheduleSpec(input); err == nil {
			t.Errorf("ParseScheduleSpec(%q) accepted", input)
		}
	}
}

func TestScheduleSpecNext(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	// 2026-10-09 is a Friday.
	friday := time.Date(2026, 10, 9, 9, 0, 0, 0, jst)
	weekdays, _ := ParseScheduleSpec("平日 09:00")
	if got, want := weekdays.Next(friday.Add(-time.Minute)), friday; !got.Equal(want) {
		t.Errorf("before the time: %v, want %v", got, want)
	}
	if got, want := weekdays.Next(friday), time.Date(2026, 10, 12, 9, 0, 0, 0, jst); !got.Equal(want) {
		t.Errorf("at the time: %v, want next Monday %v", got, want)
	}
	sunday, _ := ParseScheduleSpec("毎週日曜 08:00")
	if got, want := sunday.Next(time.Date(2026, 10, 11, 8, 0, 0, 0, jst)), time.Date(2026, 10, 18, 8, 0, 0, 0, jst); !got.Equal(want) {
		t.Errorf("weekly: %v, want a week later %v", got, want)
	}
}

func TestSplitSchedule(t *testing.T) {
	rest, request, invalid := SplitSchedule("設定します。\n\n## 定期実行\n- いつ: 平日 09:00\n- やること: CI の失敗を確認する\n\n以上")
	if invalid || request == nil || request.When != "平日 09:00" || request.Task != "CI の失敗を確認する" || request.Stop {
		t.Fatalf("SplitSchedule() = %#v, %v", request, invalid)
	}
	if rest != "設定します。\n\n以上" {
		t.Fatalf("rest = %q", rest)
	}
	if _, request, invalid := SplitSchedule("止めます。\n## 定期実行\n- いつ: 停止"); invalid || request == nil || !request.Stop {
		t.Fatalf("stop = %#v, %v", request, invalid)
	}
	if _, request, invalid := SplitSchedule("## 定期実行\n- いつ: 平日 09:00"); !invalid || request != nil {
		t.Fatalf("missing task = %#v, %v", request, invalid)
	}
	if rest, request, invalid := SplitSchedule("本文\n```\n## 定期実行\n```"); invalid || request != nil || rest != "本文\n```\n## 定期実行\n```" {
		t.Fatalf("fenced heading = %q, %#v, %v", rest, request, invalid)
	}
}
