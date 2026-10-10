package state

import (
	"testing"
	"time"
)

func TestSchedulesPersistAndAdvance(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	due := Schedule{Channel: "C1", ThreadTS: "1.1", AuthorID: "U1", Spec: "毎日 09:00", Task: "CI を確認", NextAt: now.Add(-48 * time.Hour)}
	later := Schedule{Channel: "C1", ThreadTS: "2.1", AuthorID: "U1", Spec: "毎日 10:00", Task: "後で", NextAt: now.Add(time.Hour)}
	for key, schedule := range map[string]Schedule{"C1:1.1": due, "C1:2.1": later} {
		if err := store.SetSchedule(key, schedule); err != nil {
			t.Fatalf("SetSchedule() error = %v", err)
		}
	}
	nextDay := func(Schedule) time.Time { return now.Add(24 * time.Hour) }

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	taken, err := reloaded.TakeDueSchedules(now, nextDay)
	if err != nil || len(taken) != 1 || taken[0] != due {
		t.Fatalf("TakeDueSchedules() = %#v, %v; want the due one once, though two days were missed", taken, err)
	}
	if again, _ := reloaded.TakeDueSchedules(now, nextDay); len(again) != 0 {
		t.Fatalf("second take = %#v, want none", again)
	}
	advanced, ok := reloaded.GetSchedule("C1:1.1")
	if !ok || !advanced.NextAt.Equal(now.Add(24*time.Hour)) || advanced.Runs != 1 {
		t.Fatalf("advanced = %#v, want the next occurrence and one run", advanced)
	}
	if got := reloaded.Schedules(); len(got) != 2 || got[0].ThreadTS != "2.1" {
		t.Fatalf("Schedules() = %#v, want soonest first", got)
	}
	if removed, deleted, err := reloaded.DeleteSchedule("C1:2.1"); err != nil || !deleted || removed != later {
		t.Fatalf("DeleteSchedule() = %#v, %v, %v", removed, deleted, err)
	}
	final, err := NewStore(dir)
	if err != nil {
		t.Fatalf("final reload error = %v", err)
	}
	if got := final.Schedules(); len(got) != 1 || got[0].Runs != 1 {
		t.Fatalf("persisted schedules = %#v", got)
	}
}
