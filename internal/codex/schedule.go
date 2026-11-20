package codex

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	scheduleHeading  = "## 定期実行"
	ScheduleStopWord = "停止"
)

var weekdayNames = []string{"日", "月", "火", "水", "木", "金", "土"}

// Running at most daily keeps unattended work bounded.
type ScheduleSpec struct {
	Days   [7]bool
	Hour   int
	Minute int
}

func ParseScheduleSpec(when string) (ScheduleSpec, error) {
	fields := strings.Fields(strings.ReplaceAll(strings.TrimSpace(when), "：", ":"))
	if len(fields) != 2 {
		return ScheduleSpec{}, errors.New("want days and a time")
	}
	var spec ScheduleSpec
	switch days := fields[0]; days {
	case "毎日":
		spec.Days = [7]bool{true, true, true, true, true, true, true}
	case "平日":
		spec.Days = [7]bool{false, true, true, true, true, true, false}
	case "週末":
		spec.Days = [7]bool{true, false, false, false, false, false, true}
	default:
		names, ok := strings.CutPrefix(days, "毎週")
		if !ok {
			return ScheduleSpec{}, fmt.Errorf("unknown days %q", days)
		}
		names = strings.TrimSuffix(strings.TrimSuffix(names, "日"), "曜")
		names = strings.NewReplacer("・", "", "、", "", ",", "", "曜", "").Replace(names)
		if names == "" {
			return ScheduleSpec{}, errors.New("no weekday")
		}
		for _, name := range names {
			day := -1
			for i, weekday := range weekdayNames {
				if string(name) == weekday {
					day = i
				}
			}
			if day < 0 {
				return ScheduleSpec{}, fmt.Errorf("unknown weekday %q", string(name))
			}
			spec.Days[day] = true
		}
	}
	hour, minute, ok := strings.Cut(fields[1], ":")
	if !ok {
		return ScheduleSpec{}, errors.New("time is not HH:MM")
	}
	var err error
	if spec.Hour, err = strconv.Atoi(hour); err != nil || spec.Hour < 0 || spec.Hour > 23 {
		return ScheduleSpec{}, fmt.Errorf("bad hour %q", hour)
	}
	if spec.Minute, err = strconv.Atoi(minute); err != nil || len(minute) != 2 || spec.Minute < 0 || spec.Minute > 59 {
		return ScheduleSpec{}, fmt.Errorf("bad minute %q", minute)
	}
	return spec, nil
}

func (s ScheduleSpec) String() string {
	var days string
	switch s.Days {
	case [7]bool{true, true, true, true, true, true, true}:
		days = "毎日"
	case [7]bool{false, true, true, true, true, true, false}:
		days = "平日"
	case [7]bool{true, false, false, false, false, false, true}:
		days = "週末"
	default:
		days = "毎週"
		// Monday first, as people list a week.
		for _, i := range []int{1, 2, 3, 4, 5, 6, 0} {
			if s.Days[i] {
				days += weekdayNames[i]
			}
		}
		days += "曜"
	}
	return fmt.Sprintf("%s %02d:%02d", days, s.Hour, s.Minute)
}

func (s ScheduleSpec) Next(after time.Time) time.Time {
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, after.Location())
	for i := 0; i <= 7; i++ {
		date := day.AddDate(0, 0, i)
		at := time.Date(date.Year(), date.Month(), date.Day(), s.Hour, s.Minute, 0, 0, after.Location())
		if s.Days[at.Weekday()] && at.After(after) {
			return at
		}
	}
	// Unreachable for a spec with a day: the loop covers a whole week.
	return time.Time{}
}

type ScheduleRequest struct {
	When string
	Task string
	Stop bool
}

func SplitSchedule(text string) (rest string, request *ScheduleRequest, invalid bool) {
	rest, fields, present, invalid := splitFieldSection(text, scheduleHeading, followUpWhenKey, followUpTaskKey)
	if !present {
		return text, nil, false
	}
	if invalid {
		return rest, nil, true
	}
	if fields[followUpWhenKey] == ScheduleStopWord {
		return rest, &ScheduleRequest{Stop: true}, false
	}
	if fields[followUpWhenKey] == "" || fields[followUpTaskKey] == "" {
		return rest, nil, true
	}
	return rest, &ScheduleRequest{When: fields[followUpWhenKey], Task: fields[followUpTaskKey]}, false
}
