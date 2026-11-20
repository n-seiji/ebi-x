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

type ApprovalKind string

const (
	ApprovalUser     ApprovalKind = "user"
	ApprovalChannel  ApprovalKind = "channel"
	ApprovalWorkflow ApprovalKind = "workflow"
)

type ApprovalStatus string

const (
	Pending  ApprovalStatus = "pending"
	Approved ApprovalStatus = "approved"
	Denied   ApprovalStatus = "denied"
)

type Approval struct {
	Kind             ApprovalKind   `json:"kind"`
	ID               string         `json:"id"`
	Status           ApprovalStatus `json:"status"`
	RequestedAt      time.Time      `json:"requested_at"`
	RequestedBy      string         `json:"requested_by,omitempty"`
	RequestChannel   string         `json:"request_channel,omitempty"`
	RequestThreadTS  string         `json:"request_thread_ts,omitempty"`
	RequestMessageTS string         `json:"request_message_ts,omitempty"`
	MentionTS        string         `json:"mention_ts,omitempty"`
	MentionText      string         `json:"mention_text,omitempty"`
	DecidedBy        string         `json:"decided_by,omitempty"`
	DecidedAt        time.Time      `json:"decided_at,omitzero"`
}

func (s *Store) ApprovalStatus(kind ApprovalKind, id string) ApprovalStatus {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	return s.approvals[approvalKey(kind, id)].Status
}

func (s *Store) GetApproval(kind ApprovalKind, id string) (Approval, bool) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	approval, ok := s.approvals[approvalKey(kind, id)]
	return approval, ok
}

// Reports whether this call created the request, so only one caller posts it.
func (s *Store) RequestApproval(kind ApprovalKind, id, requestedBy, requestChannel, requestThreadTS string, now time.Time) (bool, error) {
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
		Kind:            kind,
		ID:              id,
		Status:          Pending,
		RequestedAt:     now.UTC(),
		RequestedBy:     requestedBy,
		RequestChannel:  requestChannel,
		RequestThreadTS: requestThreadTS,
	}
	if err := s.saveApprovals(); err != nil {
		delete(s.approvals, key)
		return false, fmt.Errorf("save approval request %q: %w", key, err)
	}
	return true, nil
}

// The first decision wins.
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

func (s *Store) SetApprovalRequestMessage(kind ApprovalKind, id, timestamp string) error {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()

	key := approvalKey(kind, id)
	current, exists := s.approvals[key]
	if !exists {
		return fmt.Errorf("set approval request message %q: %w", key, errors.New("approval does not exist"))
	}
	next := current
	next.RequestMessageTS = timestamp
	s.approvals[key] = next
	if err := s.saveApprovals(); err != nil {
		s.approvals[key] = current
		return fmt.Errorf("save approval request message %q: %w", key, err)
	}
	return nil
}

func (s *Store) SetApprovalMention(kind ApprovalKind, id string, mention ApprovalMention) (bool, error) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()

	key := approvalKey(kind, id)
	current, exists := s.approvals[key]
	if !exists || current.Status != Pending {
		return false, nil
	}
	next := current
	next.RequestedBy = mention.User
	next.RequestChannel = mention.Channel
	next.RequestThreadTS = mention.ThreadTS
	next.MentionTS = mention.TS
	next.MentionText = mention.Text
	s.approvals[key] = next
	if err := s.saveApprovals(); err != nil {
		s.approvals[key] = current
		return false, fmt.Errorf("save approval mention %q: %w", key, err)
	}
	return true, nil
}

type ApprovalMention struct {
	User     string
	Channel  string
	ThreadTS string
	TS       string
	Text     string
}

func (s *Store) FindApprovalByRequestMessage(timestamp string) (Approval, bool) {
	if timestamp == "" {
		return Approval{}, false
	}
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	for _, approval := range s.approvals {
		if approval.RequestMessageTS == timestamp {
			return approval, true
		}
	}
	return Approval{}, false
}

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

// approvalsMu must be held.
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
