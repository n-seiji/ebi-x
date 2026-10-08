package state

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApprovalLifecycleSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)

	created, err := store.RequestApproval(ApprovalUser, "U2", "U2", "C1", "100.1", now)
	if err != nil || !created {
		t.Fatalf("RequestApproval() = %v, %v; want created", created, err)
	}
	if created, err := store.RequestApproval(ApprovalUser, "U2", "U2", "C1", "100.1", now); err != nil || created {
		t.Fatalf("second RequestApproval() = %v, %v; want not created", created, err)
	}
	approval, decided, err := store.DecideApproval(ApprovalUser, "U2", true, "UOWNER", now)
	if err != nil || !decided || approval.Status != Approved || approval.DecidedBy != "UOWNER" {
		t.Fatalf("DecideApproval() = %+v, %v, %v; want approved by UOWNER", approval, decided, err)
	}
	if _, decided, err := store.DecideApproval(ApprovalUser, "U2", false, "UOTHER", now); err != nil || decided {
		t.Fatalf("second DecideApproval() decided = %v, %v; want first decision kept", decided, err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after writes error = %v", err)
	}
	got, ok := reloaded.GetApproval(ApprovalUser, "U2")
	if !ok || got != approval {
		t.Fatalf("reloaded approval = %+v, %v; want %+v", got, ok, approval)
	}

	if deleted, err := reloaded.DeleteApproval(ApprovalUser, "U2"); err != nil || !deleted {
		t.Fatalf("DeleteApproval() = %v, %v; want deleted", deleted, err)
	}
	reloaded, err = NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() after delete error = %v", err)
	}
	if status := reloaded.ApprovalStatus(ApprovalUser, "U2"); status != "" {
		t.Fatalf("status after delete = %q, want none", status)
	}
}

func TestConcurrentApprovalRequestsAreAllSaved(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	const count = 32
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			if _, err := store.RequestApproval(ApprovalChannel, fmt.Sprintf("C%d", i), "U1", "C1", "100.1", time.Now()); err != nil {
				t.Errorf("RequestApproval() error = %v", err)
			}
		})
	}
	wg.Wait()

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if got := len(reloaded.ListApprovals()); got != count {
		t.Fatalf("reloaded approvals = %d, want %d", got, count)
	}
}

func TestApprovalRollsBackAfterSaveFailure(t *testing.T) {
	store := newTestStore(t)
	restore := blockStateDirectory(t, store.dir)

	if created, err := store.RequestApproval(ApprovalUser, "U2", "U2", "C1", "100.1", time.Now()); err == nil || created {
		t.Fatalf("RequestApproval() = %v, %v; want error", created, err)
	}
	if status := store.ApprovalStatus(ApprovalUser, "U2"); status != "" {
		t.Fatalf("status = %q, want none", status)
	}

	restore()
	mustRequestApproval(t, store, "U2")
	restore = blockStateDirectory(t, store.dir)
	if _, decided, err := store.DecideApproval(ApprovalUser, "U2", true, "UOWNER", time.Now()); err == nil || decided {
		t.Fatalf("DecideApproval() decided = %v, %v; want error", decided, err)
	}
	if status := store.ApprovalStatus(ApprovalUser, "U2"); status != Pending {
		t.Fatalf("status = %q, want pending", status)
	}
	restore()
}

func TestDecideApprovalWithoutRequest(t *testing.T) {
	store := newTestStore(t)
	approval, decided, err := store.DecideApproval(ApprovalChannel, "C9", true, "UOWNER", time.Now())
	if err != nil || !decided || approval.Status != Approved {
		t.Fatalf("DecideApproval() = %+v, %v, %v; want approved", approval, decided, err)
	}
}

func TestApprovalRequestMessage(t *testing.T) {
	store := newTestStore(t)
	mustRequestApproval(t, store, "U2")
	if err := store.SetApprovalRequestMessage(ApprovalUser, "U2", "500.1"); err != nil {
		t.Fatalf("SetApprovalRequestMessage() error = %v", err)
	}
	if approval, ok := store.FindApprovalByRequestMessage("500.1"); !ok || approval.ID != "U2" {
		t.Fatalf("FindApprovalByRequestMessage() = %+v, %v; want U2", approval, ok)
	}
	if _, ok := store.FindApprovalByRequestMessage(""); ok {
		t.Fatal("FindApprovalByRequestMessage(\"\") found an approval, want none")
	}
	if err := store.SetApprovalRequestMessage(ApprovalUser, "U9", "500.2"); err == nil {
		t.Fatal("SetApprovalRequestMessage() for unknown approval error = nil, want error")
	}
}

func TestNewStoreRejectsInvalidApprovals(t *testing.T) {
	for name, approvals := range map[string][]Approval{
		"kind":   {{Kind: "team", ID: "T1", Status: Approved}},
		"status": {{Kind: ApprovalUser, ID: "U1", Status: "maybe"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeJSONForTest(t, filepath.Join(dir, approvalsFilename), approvals)
			if _, err := NewStore(dir); err == nil || !strings.Contains(err.Error(), "load approvals") {
				t.Fatalf("NewStore() error = %v, want load approvals error", err)
			}
		})
	}
}

func TestLockDirRejectsSecondHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := LockDir(dir)
	if err != nil {
		t.Fatalf("LockDir() error = %v", err)
	}
	if _, err := LockDir(dir); err == nil || !strings.Contains(err.Error(), "another ebi-x process") {
		t.Fatalf("second LockDir() error = %v, want lock conflict", err)
	}
	release()
	release, err = LockDir(dir)
	if err != nil {
		t.Fatalf("LockDir() after release error = %v", err)
	}
	release()
}

func mustRequestApproval(t *testing.T, store *Store, id string) {
	t.Helper()
	if created, err := store.RequestApproval(ApprovalUser, id, id, "C1", "100.1", time.Now()); err != nil || !created {
		t.Fatalf("RequestApproval(%q) = %v, %v; want created", id, created, err)
	}
}
