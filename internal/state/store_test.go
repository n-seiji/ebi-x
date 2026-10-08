package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimEventConcurrent(t *testing.T) {
	store := newTestStore(t)
	const goroutines = 32

	start := make(chan struct{})
	var claimedCount atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Go(func() {
			<-start
			claimed, err := store.ClaimEvent("C123:100.001")
			if err != nil {
				errs <- err
				return
			}
			if claimed {
				claimedCount.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("ClaimEvent() error = %v, want nil", err)
	}
	if got, want := claimedCount.Load(), int32(1); got != want {
		t.Errorf("ClaimEvent() claimed count = %d, want %d", got, want)
	}
}

func TestClaimEventRollsBackAfterSaveFailure(t *testing.T) {
	store := newTestStore(t)
	restore := blockStateDirectory(t, store.dir)

	claimed, err := store.ClaimEvent("C123:100.002")
	if err == nil {
		t.Errorf("ClaimEvent() error = %v, want non-nil", err)
	}
	if got, want := claimed, false; got != want {
		t.Errorf("ClaimEvent() claimed = %v, want %v", got, want)
	}

	restore()
	claimed, err = store.ClaimEvent("C123:100.002")
	if err != nil {
		t.Fatalf("ClaimEvent() after restore error = %v, want nil", err)
	}
	if got, want := claimed, true; got != want {
		t.Errorf("ClaimEvent() after restore claimed = %v, want %v", got, want)
	}
}

func TestTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    State
		to      State
		wantErr bool
	}{
		{name: "allowed", from: Received, to: Planning, wantErr: false},
		{name: "single turn", from: Received, to: Working, wantErr: false},
		{name: "pre-execution failure", from: Received, to: Failed, wantErr: false},
		{name: "disallowed skip to done", from: Received, to: Done, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			mustClaim(t, store, "C123:200.001")
			err := store.Transition("C123:200.001", tt.from, tt.to)
			if got := err != nil; got != tt.wantErr {
				t.Errorf("Transition() error present = %v, want %v (error = %v)", got, tt.wantErr, err)
			}
		})
	}
}

func TestTransitionRejectsCASMismatch(t *testing.T) {
	store := newTestStore(t)
	mustClaim(t, store, "C123:200.002")

	err := store.Transition("C123:200.002", Planning, PlanPosted)
	if err == nil {
		t.Errorf("Transition() error = %v, want non-nil", err)
	}
}

func TestRecoverStartup(t *testing.T) {
	tests := []struct {
		name string
		from State
		want State
	}{
		{name: "received", from: Received, want: Failed},
		{name: "planning", from: Planning, want: Failed},
		{name: "plan posted", from: PlanPosted, want: Failed},
		{name: "working", from: Working, want: Interrupted},
		{name: "done unchanged", from: Done, want: Done},
		{name: "failed unchanged", from: Failed, want: Failed},
		{name: "interrupted unchanged", from: Interrupted, want: Interrupted},
	}

	dir := t.TempDir()
	entries := make(map[string]event, len(tests))
	for _, tt := range tests {
		entries[tt.name] = event{State: tt.from, UpdatedAt: time.Now().Add(-time.Hour)}
	}
	writeJSONForTest(t, filepath.Join(dir, eventsFilename), entries)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if err := store.RecoverStartup(); err != nil {
		t.Fatalf("RecoverStartup() error = %v, want nil", err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after recovery error = %v, want nil", err)
	}
	for _, tt := range tests {
		if got := reloaded.events[tt.name].State; got != tt.want {
			t.Errorf("RecoverStartup() state for %q = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRecoverStartupSaveFailure(t *testing.T) {
	store := newTestStore(t)
	mustClaim(t, store, "C123:300.001")
	restore := blockStateDirectory(t, store.dir)

	err := store.RecoverStartup()
	if err == nil {
		t.Errorf("RecoverStartup() error = %v, want non-nil", err)
	}
	if got, want := store.events["C123:300.001"].State, Received; got != want {
		t.Errorf("RecoverStartup() state after save failure = %q, want %q", got, want)
	}
	restore()
}

func TestGC(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-6 * 24 * time.Hour)
	tests := []struct {
		name      string
		state     State
		updatedAt time.Time
		wantKept  bool
	}{
		{name: "old done", state: Done, updatedAt: old, wantKept: false},
		{name: "old failed", state: Failed, updatedAt: old, wantKept: false},
		{name: "old interrupted", state: Interrupted, updatedAt: old, wantKept: false},
		{name: "recent done", state: Done, updatedAt: recent, wantKept: true},
		{name: "old received", state: Received, updatedAt: old, wantKept: true},
		{name: "old planning", state: Planning, updatedAt: old, wantKept: true},
		{name: "old plan posted", state: PlanPosted, updatedAt: old, wantKept: true},
		{name: "old working", state: Working, updatedAt: old, wantKept: true},
	}

	dir := t.TempDir()
	entries := make(map[string]event, len(tests))
	for _, tt := range tests {
		entries[tt.name] = event{State: tt.state, UpdatedAt: tt.updatedAt}
	}
	writeJSONForTest(t, filepath.Join(dir, eventsFilename), entries)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if err := store.GC(now, 7*24*time.Hour); err != nil {
		t.Fatalf("GC() error = %v, want nil", err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after GC error = %v, want nil", err)
	}
	for _, tt := range tests {
		_, gotKept := reloaded.events[tt.name]
		if gotKept != tt.wantKept {
			t.Errorf("GC() kept %q = %v, want %v", tt.name, gotKept, tt.wantKept)
		}
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}

	startedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	expiresAt := startedAt.Add(24 * time.Hour)
	if err := store.SetSubscription("C123:500.001", startedAt, expiresAt); err != nil {
		t.Fatalf("SetSubscription() error = %v, want nil", err)
	}
	if got, ok := store.GetSubscription("C123:500.001"); !ok || got != (Subscription{StartedAt: startedAt, ExpiresAt: expiresAt}) {
		t.Errorf("GetSubscription() = (%+v, %v), want (%+v, true)", got, ok, Subscription{StartedAt: startedAt, ExpiresAt: expiresAt})
	}

	renewedAt := expiresAt.Add(24 * time.Hour)
	if err := store.SetSubscription("C123:500.001", startedAt, renewedAt); err != nil {
		t.Fatalf("SetSubscription() renewal error = %v, want nil", err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after subscription writes error = %v, want nil", err)
	}
	if got, ok := reloaded.GetSubscription("C123:500.001"); !ok || got != (Subscription{StartedAt: startedAt, ExpiresAt: renewedAt}) {
		t.Errorf("reloaded GetSubscription() = (%+v, %v), want (%+v, true)", got, ok, Subscription{StartedAt: startedAt, ExpiresAt: renewedAt})
	}

	if err := reloaded.DeleteSubscription("C123:500.001"); err != nil {
		t.Fatalf("DeleteSubscription() error = %v, want nil", err)
	}
	if _, ok := reloaded.GetSubscription("C123:500.001"); ok {
		t.Error("GetSubscription() after DeleteSubscription() found a subscription, want none")
	}
	reloaded, err = NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after DeleteSubscription() error = %v, want nil", err)
	}
	if _, ok := reloaded.GetSubscription("C123:500.001"); ok {
		t.Error("reloaded GetSubscription() after DeleteSubscription() found a subscription, want none")
	}
}

func TestDeleteSubscriptionIfExpired(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		expiresAt   *time.Time
		wantDeleted bool
		wantFound   bool
	}{
		{name: "missing", wantDeleted: false, wantFound: false},
		{name: "active", expiresAt: new(now.Add(time.Second)), wantDeleted: false, wantFound: true},
		{name: "boundary", expiresAt: new(now), wantDeleted: true, wantFound: false},
		{name: "expired", expiresAt: new(now.Add(-time.Second)), wantDeleted: true, wantFound: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewStore(dir)
			if err != nil {
				t.Fatalf("NewStore() error = %v, want nil", err)
			}
			const key = "C123:550.001"
			if test.expiresAt != nil {
				if err := store.SetSubscription(key, now.Add(-time.Hour), *test.expiresAt); err != nil {
					t.Fatalf("SetSubscription() error = %v, want nil", err)
				}
			}

			deleted, err := store.DeleteSubscriptionIfExpired(key, now)
			if err != nil {
				t.Fatalf("DeleteSubscriptionIfExpired() error = %v, want nil", err)
			}
			if deleted != test.wantDeleted {
				t.Errorf("DeleteSubscriptionIfExpired() deleted = %v, want %v", deleted, test.wantDeleted)
			}
			_, found := store.GetSubscription(key)
			if found != test.wantFound {
				t.Errorf("GetSubscription() found = %v after conditional delete, want %v", found, test.wantFound)
			}

			reloaded, err := NewStore(dir)
			if err != nil {
				t.Fatalf("NewStore() after conditional delete error = %v, want nil", err)
			}
			_, found = reloaded.GetSubscription(key)
			if found != test.wantFound {
				t.Errorf("reloaded GetSubscription() found = %v, want %v", found, test.wantFound)
			}
		})
	}
}

func TestDeleteSubscriptionIfExpiredRollsBackAfterSaveFailure(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	const key = "C123:550.002"
	want := Subscription{StartedAt: now.Add(-time.Hour), ExpiresAt: now}
	if err := store.SetSubscription(key, want.StartedAt, want.ExpiresAt); err != nil {
		t.Fatalf("SetSubscription() error = %v, want nil", err)
	}
	restore := blockStateDirectory(t, store.dir)

	deleted, err := store.DeleteSubscriptionIfExpired(key, now)
	if err == nil {
		t.Fatal("DeleteSubscriptionIfExpired() error = nil, want non-nil")
	}
	if deleted {
		t.Fatal("DeleteSubscriptionIfExpired() deleted = true after save failure, want false")
	}
	if got, ok := store.GetSubscription(key); !ok || got != want {
		t.Fatalf("subscription after save failure = (%+v, %v), want rollback (%+v, true)", got, ok, want)
	}

	restore()
	deleted, err = store.DeleteSubscriptionIfExpired(key, now)
	if err != nil || !deleted {
		t.Fatalf("DeleteSubscriptionIfExpired() after restore = (%v, %v), want (true, nil)", deleted, err)
	}
}

func TestGCRemovesExpiredSubscriptions(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if err := store.SetSubscription("C123:600.001", now.Add(-2*time.Hour), now.Add(-time.Second)); err != nil {
		t.Fatalf("SetSubscription() expired error = %v, want nil", err)
	}
	if err := store.SetSubscription("C123:600.002", now.Add(-time.Hour), now); err != nil {
		t.Fatalf("SetSubscription() boundary error = %v, want nil", err)
	}
	if err := store.SetSubscription("C123:600.003", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("SetSubscription() active error = %v, want nil", err)
	}

	if err := store.GC(now, 7*24*time.Hour); err != nil {
		t.Fatalf("GC() error = %v, want nil", err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after GC error = %v, want nil", err)
	}
	if _, ok := reloaded.GetSubscription("C123:600.001"); ok {
		t.Error("GC() retained expired subscription, want removed")
	}
	if _, ok := reloaded.GetSubscription("C123:600.002"); ok {
		t.Error("GC() retained boundary-expired subscription, want removed")
	}
	if _, ok := reloaded.GetSubscription("C123:600.003"); !ok {
		t.Error("GC() removed active subscription, want retained")
	}
}

func TestGCKeepsEventCleanupAfterSubscriptionSaveFailure(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	store.events["C123:700.001"] = event{State: Done, UpdatedAt: now.Add(-8 * 24 * time.Hour)}
	if err := store.saveEvents(); err != nil {
		t.Fatalf("saveEvents() setup error = %v, want nil", err)
	}
	if err := store.SetSubscription("C123:700.002", now.Add(-time.Hour), now.Add(-time.Second)); err != nil {
		t.Fatalf("SetSubscription() setup error = %v, want nil", err)
	}

	subscriptionsPath := filepath.Join(dir, subscriptionsFilename)
	if err := os.Remove(subscriptionsPath); err != nil {
		t.Fatalf("Remove() subscriptions file error = %v, want nil", err)
	}
	if err := os.Mkdir(subscriptionsPath, 0o700); err != nil {
		t.Fatalf("Mkdir() subscriptions blocker error = %v, want nil", err)
	}

	err = store.GC(now, 7*24*time.Hour)
	if err == nil {
		t.Fatal("GC() error = nil, want non-nil")
	}
	if _, ok := store.events["C123:700.001"]; ok {
		t.Error("GC() restored an event whose cleanup was already saved")
	}
	if _, ok := store.GetSubscription("C123:700.002"); !ok {
		t.Error("GC() did not restore the subscription whose save failed")
	}

	if err := os.Remove(subscriptionsPath); err != nil {
		t.Fatalf("Remove() subscriptions blocker error = %v, want nil", err)
	}
	if err := store.GC(now, 7*24*time.Hour); err != nil {
		t.Fatalf("GC() retry error = %v, want nil", err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after GC retry error = %v, want nil", err)
	}
	if _, ok := reloaded.events["C123:700.001"]; ok {
		t.Error("reloaded event found after GC cleanup, want none")
	}
	if _, ok := reloaded.GetSubscription("C123:700.002"); ok {
		t.Error("reloaded subscription found after GC retry, want none")
	}
}

func TestNewStoreRejectsCorruptJSON(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		wantErr  string
	}{
		{name: "sessions", filename: sessionsFilename, wantErr: "load sessions"},
		{name: "events", filename: eventsFilename, wantErr: "load events"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.filename), []byte("{broken"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v, want nil", err)
			}
			_, err := NewStore(dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewStore() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAtomicWritesSurviveReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if err := store.SetThread("C123:400.001", "thread-123"); err != nil {
		t.Fatalf("SetThread() error = %v, want nil", err)
	}
	mustClaim(t, store, "C123:400.002")
	if err := store.Transition("C123:400.002", Received, Planning); err != nil {
		t.Fatalf("Transition() error = %v, want nil", err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after writes error = %v, want nil", err)
	}
	threadID, ok := reloaded.GetThread("C123:400.001")
	if got, want := threadID, "thread-123"; got != want {
		t.Errorf("GetThread() threadID = %q, want %q", got, want)
	}
	if got, want := ok, true; got != want {
		t.Errorf("GetThread() ok = %v, want %v", got, want)
	}
	if got, want := reloaded.events["C123:400.002"].State, Planning; got != want {
		t.Errorf("reloaded event state = %q, want %q", got, want)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	return store
}

//go:fix inline
func timePointer(value time.Time) *time.Time {
	return new(value)
}

func mustClaim(t *testing.T, store *Store, key string) {
	t.Helper()
	claimed, err := store.ClaimEvent(key)
	if err != nil {
		t.Fatalf("ClaimEvent() error = %v, want nil", err)
	}
	if got, want := claimed, true; got != want {
		t.Fatalf("ClaimEvent() claimed = %v, want %v", got, want)
	}
}

func blockStateDirectory(t *testing.T, dir string) func() {
	t.Helper()
	backup := dir + "-backup"
	if err := os.Rename(dir, backup); err != nil {
		t.Fatalf("Rename() state directory error = %v, want nil", err)
	}
	if err := os.WriteFile(dir, []byte("blocks directory recreation"), 0o600); err != nil {
		t.Fatalf("WriteFile() blocker error = %v, want nil", err)
	}
	restored := false
	restore := func() {
		t.Helper()
		if restored {
			return
		}
		if err := os.Remove(dir); err != nil {
			t.Fatalf("Remove() blocker error = %v, want nil", err)
		}
		if err := os.Rename(backup, dir); err != nil {
			t.Fatalf("Rename() restore error = %v, want nil", err)
		}
		restored = true
	}
	t.Cleanup(restore)
	return restore
}

func writeJSONForTest(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v, want nil", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v, want nil", err)
	}
}
