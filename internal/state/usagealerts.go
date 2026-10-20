package state

import (
	"fmt"
	"path/filepath"
)

const usageAlertsFilename = "usage_alerts.json"

// UsageAlerts returns, for each Codex usage window, the lowest remaining
// percentage already announced in the current window.
func (s *Store) UsageAlerts() map[string]int {
	s.usageAlertsMu.Lock()
	defer s.usageAlertsMu.Unlock()
	alerts := make(map[string]int, len(s.usageAlerts))
	for window, remaining := range s.usageAlerts {
		alerts[window] = remaining
	}
	return alerts
}

// SetUsageAlerts replaces the announced levels, so a restart does not repeat
// an announcement.
func (s *Store) SetUsageAlerts(alerts map[string]int) error {
	s.usageAlertsMu.Lock()
	defer s.usageAlertsMu.Unlock()
	previous := s.usageAlerts
	s.usageAlerts = make(map[string]int, len(alerts))
	for window, remaining := range alerts {
		s.usageAlerts[window] = remaining
	}
	if err := atomicWriteJSON(filepath.Join(s.dir, usageAlertsFilename), s.usageAlerts); err != nil {
		s.usageAlerts = previous
		return fmt.Errorf("write usage alerts: %w", err)
	}
	return nil
}
