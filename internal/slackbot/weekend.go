package slackbot

import "time"

// isWeekend reports whether at is a Saturday or Sunday in local time.
func isWeekend(at time.Time) bool {
	weekday := at.In(time.Local).Weekday()
	return weekday == time.Saturday || weekday == time.Sunday
}
