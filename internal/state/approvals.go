package state

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ApprovalKind is the kind of Slack entity an approval applies to.
type ApprovalKind string

const (
	// ApprovalUser approves a Slack user.
	ApprovalUser ApprovalKind = "user"
	// ApprovalChannel approves a Slack channel.
	ApprovalChannel ApprovalKind = "channel"
	// ApprovalWorkflow approves a Slack workflow.
	ApprovalWorkflow ApprovalKind = "workflow"
)

// ApprovalStatus is the decision recorded for an approval.
type ApprovalStatus string

const (
	// Pending means a request was posted and nobody has decided yet.
	Pending ApprovalStatus = "pending"
	// Approved means the entity may use the bot.
	Approved ApprovalStatus = "approved"
	// Denied means the entity is rejected without asking again.
	Denied ApprovalStatus = "denied"
)

// Approval is the recorded access decision for one Slack entity.
type Approval struct {
	Kind        ApprovalKind   `json:"kind"`
	ID          string         `json:"id"`
	Status      ApprovalStatus `json:"status"`
	RequestedAt time.Time      `json:"requested_at"`
	// RequestedBy is the user whose mention triggered the request.
	RequestedBy string `json:"requested_by,omitempty"`
	// RequestChannel is the channel the triggering mention was posted in.
	RequestChannel string    `json:"request_channel,omitempty"`
	DecidedBy      string    `json:"decided_by,omitempty"`
	DecidedAt      time.Time `json:"decided_at,omitzero"`
}

// ApprovalStatus returns the recorded status of kind and id, or "" when
// nothing is recorded.
func (s *Store) ApprovalStatus(kind ApprovalKind, id string) ApprovalStatus {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	return s.approvals[approvalKey(kind, id)].Status
}

// GetApproval returns the recorded approval of kind and id.
func (s *Store) GetApproval(kind ApprovalKind, id string) (Approval, bool) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	approval, ok := s.approvals[approvalKey(kind, id)]
	return approval, ok
}

// ListApprovals returns every recorded approval ordered by kind and ID.
func (s *Store) ListApprovals() []Approval {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	return s.sortedApprovals()
}

// RequestApproval records a pending request for kind and id unless something
// is already recorded for them. It reports whether this call created the
// request, so only one caller posts it.
func (s *Store) RequestApproval(kind ApprovalKind, id, requestedBy, requestChannel string, now time.Time) (bool, error) {
	if err := validApprovalKind(kind); err != nil {
		return false, err
	}
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()

	key := approvalKey(kind, id)
	if _, exists := s.approvals[key]; exists {
		return false, nil
	}
	s.approvals[key] = Approval{
		Kind:           kind,
		ID:             id,
		Status:         Pending,
		RequestedAt:    now.UTC(),
		RequestedBy:    requestedBy,
		RequestChannel: requestChannel,
	}
	if err := s.saveApprovals(); err != nil {
		delete(s.approvals, key)
		return false, fmt.Errorf("save approval request %q: %w", key, err)
	}
	return true, nil
}

// DecideApproval approves or denies a pending request. When the request is
// no longer pending, it reports false with the current record (ok is false
// when nothing is recorded), so the first decision wins.
func (s *Store) DecideApproval(kind ApprovalKind, id string, approve bool, decidedBy string, now time.Time) (approval Approval, decided bool, err error) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()

	key := approvalKey(kind, id)
	current, exists := s.approvals[key]
	if !exists || current.Status != Pending {
		return current, false, nil
	}
	next := current
	next.Status = Denied
	if approve {
		next.Status = Approved
	}
	next.DecidedBy = decidedBy
	next.DecidedAt = now.UTC()
	s.approvals[key] = next
	if err := s.saveApprovals(); err != nil {
		s.approvals[key] = current
		return current, false, fmt.Errorf("save approval decision %q: %w", key, err)
	}
	return next, true, nil
}

// DeleteApproval removes whatever is recorded for kind and id, so the next
// mention asks again. It reports whether a record existed.
func (s *Store) DeleteApproval(kind ApprovalKind, id string) (bool, error) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()

	key := approvalKey(kind, id)
	previous, exists := s.approvals[key]
	if !exists {
		return false, nil
	}
	delete(s.approvals, key)
	if err := s.saveApprovals(); err != nil {
		s.approvals[key] = previous
		return false, fmt.Errorf("delete approval %q: %w", key, err)
	}
	return true, nil
}

func (s *Store) saveApprovals() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, approvalsFilename), s.sortedApprovals()); err != nil {
		return fmt.Errorf("write approvals: %w", err)
	}
	return nil
}

// sortedApprovals returns a stable copy of the approvals; approvalsMu must be
// held.
func (s *Store) sortedApprovals() []Approval {
	approvals := slices.Collect(maps.Values(s.approvals))
	slices.SortFunc(approvals, func(a, b Approval) int {
		return strings.Compare(approvalKey(a.Kind, a.ID), approvalKey(b.Kind, b.ID))
	})
	return approvals
}

func loadApprovals(path string) (map[string]Approval, error) {
	var list []Approval
	if err := loadJSON(path, &list); err != nil {
		return nil, err
	}
	approvals := make(map[string]Approval, len(list))
	for _, approval := range list {
		if err := validApprovalKind(approval.Kind); err != nil {
			return nil, err
		}
		switch approval.Status {
		case Pending, Approved, Denied:
		default:
			return nil, fmt.Errorf("approval %s:%s has invalid status %q", approval.Kind, approval.ID, approval.Status)
		}
		approvals[approvalKey(approval.Kind, approval.ID)] = approval
	}
	return approvals, nil
}

func validApprovalKind(kind ApprovalKind) error {
	switch kind {
	case ApprovalUser, ApprovalChannel, ApprovalWorkflow:
		return nil
	}
	return fmt.Errorf("approval kind %q: %w", kind, errors.New("unknown kind"))
}

func approvalKey(kind ApprovalKind, id string) string {
	return string(kind) + ":" + id
}
