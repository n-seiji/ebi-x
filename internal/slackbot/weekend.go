package slackbot

import "time"

func isWeekend(at time.Time) bool {
	weekday := at.In(time.Local).Weekday()
	return weekday == time.Saturday || weekday == time.Sunday
}
