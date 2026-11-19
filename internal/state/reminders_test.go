package state

import (
	"testing"
	"time"
)

func TestRemindersPersistAndFireOnce(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	due := Reminder{ID: "C1:1.1:1", Channel: "C1", ThreadTS: "1.1", AuthorID: "U1", Text: "資料を送る", DueAt: now.Add(-time.Hour)}
	later := Reminder{ID: "C1:1.1:2", Channel: "C1", ThreadTS: "1.1", AuthorID: "U1", Text: "会議", DueAt: now.Add(time.Hour)}
	other := Reminder{ID: "C2:2.1:1", Channel: "C2", ThreadTS: "2.1", AuthorID: "U2", Text: "別件", DueAt: now.Add(2 * time.Hour)}
	for _, reminder := range []Reminder{later, due, other} {
		if err := store.AddReminder(reminder); err != nil {
			t.Fatalf("AddReminder() error = %v", err)
		}
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	if got := reloaded.Reminders(); len(got) != 3 || got[0] != due || got[2] != other {
		t.Fatalf("Reminders() = %#v, want all three earliest first", got)
	}
	taken, err := reloaded.TakeDueReminders(now)
	if err != nil || len(taken) != 1 || taken[0] != due {
		t.Fatalf("TakeDueReminders() = %#v, %v; want the due one", taken, err)
	}
	if again, _ := reloaded.TakeDueReminders(now); len(again) != 0 {
		t.Fatalf("second take = %#v, want none", again)
	}
	removed, err := reloaded.DeleteReminders(func(r Reminder) bool { return r.Channel == "C1" })
	if err != nil || len(removed) != 1 || removed[0] != later {
		t.Fatalf("DeleteReminders() = %#v, %v; want the thread's pending one", removed, err)
	}
	final, err := NewStore(dir)
	if err != nil {
		t.Fatalf("final reload error = %v", err)
	}
	if got := final.Reminders(); len(got) != 1 || got[0] != other {
		t.Fatalf("after delete = %#v, want only the other thread's", got)
	}
}
