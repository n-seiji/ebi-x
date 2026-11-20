package state

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"
)

const usageFilename = "usage.json"

type Usage struct {
	Alerts   map[string]int      `json:"alerts,omitempty"`
	Day      string              `json:"day,omitempty"`
	Windows  map[string]UsageDay `json:"windows,omitempty"`
	History  []UsageSample       `json:"history,omitempty"`
	Reported *UsageSample        `json:"reported,omitempty"`
}

type UsageSample struct {
	At        time.Time          `json:"at"`
	Remaining map[string]float64 `json:"remaining"`
	ResetsAt  map[string]int64   `json:"resetsAt,omitempty"`
	Minutes   map[string]int64   `json:"minutes,omitempty"`
}

type UsageDay struct {
	Label  string  `json:"label"`
	First  float64 `json:"first"`
	Last   float64 `json:"last"`
	Min    float64 `json:"min"`
	Checks int     `json:"checks"`
}

func (s *Store) Usage() Usage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return cloneUsage(s.usage)
}

func (s *Store) SetUsage(usage Usage) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	previous := s.usage
	s.usage = cloneUsage(usage)
	if err := atomicWriteJSON(filepath.Join(s.dir, usageFilename), s.usage); err != nil {
		s.usage = previous
		return fmt.Errorf("write usage: %w", err)
	}
	return nil
}

func cloneUsage(usage Usage) Usage {
	usage.Alerts = maps.Clone(usage.Alerts)
	usage.Windows = maps.Clone(usage.Windows)
	usage.History = slices.Clone(usage.History)
	for i, sample := range usage.History {
		usage.History[i] = cloneUsageSample(sample)
	}
	if usage.Reported != nil {
		reported := cloneUsageSample(*usage.Reported)
		usage.Reported = &reported
	}
	return usage
}

func cloneUsageSample(sample UsageSample) UsageSample {
	sample.Remaining = maps.Clone(sample.Remaining)
	sample.ResetsAt = maps.Clone(sample.ResetsAt)
	sample.Minutes = maps.Clone(sample.Minutes)
	return sample
}
