package state

import (
	"testing"
	"time"
)

func TestFollowUpsPersistAndTakeDue(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC)
	due := FollowUp{Channel: "C1", ThreadTS: "1.1", AuthorID: "U1", Task: "確認", DueAt: now.Add(-time.Minute), Chain: 1}
	later := FollowUp{Channel: "C1", ThreadTS: "2.1", AuthorID: "U1", Task: "後で", DueAt: now.Add(time.Hour), Chain: 1}
	for key, followUp := range map[string]FollowUp{"C1:1.1": due, "C1:2.1": later} {
		if err := store.SetFollowUp(key, followUp); err != nil {
			t.Fatalf("SetFollowUp() error = %v", err)
		}
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	taken, err := reloaded.TakeDueFollowUps(now)
	if err != nil || len(taken) != 1 || taken[0] != due {
		t.Fatalf("TakeDueFollowUps() = %#v, %v; want only the due one", taken, err)
	}
	if again, _ := reloaded.TakeDueFollowUps(now); len(again) != 0 {
		t.Fatalf("second take = %#v, want none", again)
	}
	if got, ok := reloaded.GetFollowUp("C1:2.1"); !ok || got != later {
		t.Fatalf("GetFollowUp() = %#v, %v", got, ok)
	}
	if deleted, err := reloaded.DeleteFollowUp("C1:2.1"); err != nil || !deleted {
		t.Fatalf("DeleteFollowUp() = %v, %v", deleted, err)
	}
	final, err := NewStore(dir)
	if err != nil {
		t.Fatalf("final reload error = %v", err)
	}
	if _, ok := final.GetFollowUp("C1:2.1"); ok {
		t.Fatal("deleted follow-up came back after reload")
	}
}
