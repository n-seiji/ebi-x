package state

import (
	"fmt"
	"maps"
	"path/filepath"
)

const usageFilename = "usage.json"

// Usage is what the bot remembers about the Codex account's usage between
// checks and restarts.
type Usage struct {
	// Alerts holds, for each usage window, the lowest remaining level
	// already announced in the window's current period.
	Alerts map[string]int `json:"alerts,omitempty"`
	// Day is the local date (YYYY-MM-DD) Windows describes.
	Day string `json:"day,omitempty"`
	// Windows summarizes each window's checks on Day.
	Windows map[string]UsageDay `json:"windows,omitempty"`
}

// UsageDay summarizes one usage window's checks during a day, in remaining
// percent.
type UsageDay struct {
	Label  string  `json:"label"`
	First  float64 `json:"first"`
	Last   float64 `json:"last"`
	Min    float64 `json:"min"`
	Checks int     `json:"checks"`
}

// Usage returns the remembered usage state.
func (s *Store) Usage() Usage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return cloneUsage(s.usage)
}

// SetUsage replaces the remembered usage state.
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
	return usage
}
