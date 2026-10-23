package state

import (
	"testing"
	"time"
)

func TestDeferredPersistAndTake(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	first := Deferred{Source: "mention", AuthorID: "U1", Channel: "C1", Timestamp: "1.2", ThreadTS: "1.1", Message: "a", DeferredAt: now}
	second := Deferred{Source: "message", AuthorID: "U1", Channel: "C1", Timestamp: "1.3", ThreadTS: "1.1", Message: "b", ThreadReply: true, DeferredAt: now.Add(time.Second)}
	other := Deferred{Source: "mention", AuthorID: "U2", Channel: "C2", Timestamp: "2.1", ThreadTS: "2.1", Message: "c", DeferredAt: now.Add(2 * time.Second)}
	for _, deferred := range []Deferred{second, first, other} {
		if err := store.AddDeferred(deferred); err != nil {
			t.Fatalf("AddDeferred() error = %v", err)
		}
	}
	// The same event arriving again, as a mention and a message, is kept once.
	duplicate := first
	duplicate.Source = "message"
	if err := store.AddDeferred(duplicate); err != nil {
		t.Fatalf("AddDeferred(duplicate) error = %v", err)
	}
	if removed, err := store.DeleteDeferred(func(d Deferred) bool { return d.Channel == "C2" }); err != nil || removed != 1 {
		t.Fatalf("DeleteDeferred() = %d, %v; want 1", removed, err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	taken, err := reloaded.TakeDeferred()
	if err != nil || len(taken) != 2 || taken[0] != first || taken[1] != second {
		t.Fatalf("TakeDeferred() = %#v, %v; want first then second", taken, err)
	}
	final, err := NewStore(dir)
	if err != nil {
		t.Fatalf("final reload error = %v", err)
	}
	if again, _ := final.TakeDeferred(); len(again) != 0 {
		t.Fatalf("second take = %#v, want none", again)
	}
}
