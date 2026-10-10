package state

import (
	"testing"
	"time"
)

func TestPullWatchesPersist(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	first := PullWatch{Channel: "C1", ThreadTS: "1.0", Owner: "o", Repo: "r", Number: 1, Cursor: PullCursor{Review: 3}, CreatedAt: now}
	second := PullWatch{Channel: "C1", ThreadTS: "2.0", Owner: "o", Repo: "r", Number: 2, CreatedAt: now.Add(time.Minute)}
	for _, watch := range []PullWatch{second, first} {
		if err := store.SetPullWatch(watch); err != nil {
			t.Fatal(err)
		}
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	watches := reloaded.PullWatches()
	if len(watches) != 2 || watches[0].Key() != "o/r#1" || watches[1].Key() != "o/r#2" || watches[0].Cursor.Review != 3 {
		t.Fatalf("watches = %+v, want both, oldest first", watches)
	}

	removed, err := reloaded.DeletePullWatches(func(w PullWatch) bool { return w.ThreadTS == "1.0" })
	if err != nil || len(removed) != 1 || removed[0].Key() != "o/r#1" {
		t.Fatalf("DeletePullWatches() = %+v, %v", removed, err)
	}
	if _, ok := reloaded.GetPullWatch("o/r#1"); ok {
		t.Fatal("deleted watch is still there")
	}
	again, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.PullWatches(); len(got) != 1 || got[0].Key() != "o/r#2" {
		t.Fatalf("watches after delete = %+v", got)
	}
}

func TestFollowUpsAreListedEarliestFirst(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	_ = store.SetFollowUp("C1:2", FollowUp{Channel: "C1", ThreadTS: "2", DueAt: now.Add(2 * time.Hour)})
	_ = store.SetFollowUp("C1:1", FollowUp{Channel: "C1", ThreadTS: "1", DueAt: now.Add(time.Hour)})
	if got := store.FollowUps(); len(got) != 2 || got[0].ThreadTS != "1" || got[1].ThreadTS != "2" {
		t.Fatalf("FollowUps() = %+v, want earliest first", got)
	}
}
