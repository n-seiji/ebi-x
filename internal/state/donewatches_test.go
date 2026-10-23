package state

import (
	"testing"
	"time"
)

func TestDoneWatchesPersist(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	first := DoneWatch{Channel: "C1", ThreadTS: "1.0", MessageTS: "1.0", Targets: []string{"U2", "U3"}, IntervalDays: 2, CreatedAt: now}
	second := DoneWatch{Channel: "C1", ThreadTS: "2.0", MessageTS: "2.5", Targets: []string{"U4"}, CreatedAt: now.Add(time.Minute)}
	for _, watch := range []DoneWatch{second, first} {
		if err := store.SetDoneWatch(watch); err != nil {
			t.Fatal(err)
		}
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	watches := reloaded.DoneWatches()
	if len(watches) != 2 || watches[0].Key() != "C1:1.0" || watches[1].Key() != "C1:2.5" || len(watches[0].Targets) != 2 {
		t.Fatalf("watches = %+v, want both, oldest first", watches)
	}

	removed, err := reloaded.DeleteDoneWatches(func(w DoneWatch) bool { return w.ThreadTS == "1.0" })
	if err != nil || len(removed) != 1 || removed[0].Key() != "C1:1.0" {
		t.Fatalf("DeleteDoneWatches() = %+v, %v", removed, err)
	}
	again, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if watches := again.DoneWatches(); len(watches) != 1 || watches[0].Key() != "C1:2.5" {
		t.Fatalf("watches after delete = %+v", watches)
	}
}

func TestUpdateDoneWatchSkipsMissingWatch(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDoneWatch(DoneWatch{Channel: "C1", ThreadTS: "1.0", MessageTS: "1.0", Targets: []string{"U2"}}); err != nil {
		t.Fatal(err)
	}
	bump := func(w *DoneWatch) bool { w.Reminders++; return true }

	if watch, ok, err := store.UpdateDoneWatch("C1:1.0", bump); err != nil || !ok || watch.Reminders != 1 {
		t.Fatalf("UpdateDoneWatch() = %+v, %v, %v; want one reminder", watch, ok, err)
	}
	if _, ok, err := store.UpdateDoneWatch("C1:1.0", func(*DoneWatch) bool { return false }); err != nil || ok {
		t.Fatalf("declined UpdateDoneWatch() = %v, %v; want not saved", ok, err)
	}
	if _, err := store.DeleteDoneWatches(func(DoneWatch) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.UpdateDoneWatch("C1:1.0", bump); err != nil || ok {
		t.Fatalf("UpdateDoneWatch() after delete = %v, %v; want nothing", ok, err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if watches := reloaded.DoneWatches(); len(watches) != 0 {
		t.Fatalf("watches = %+v, want none after the stop", watches)
	}
}
