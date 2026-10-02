// Package cron parses five-field cron expressions and computes the next firing time.
//
// The fields are, in order: minute, hour, day of month, month and day of week.
// Each field accepts "*", plain numbers, comma lists, a-b ranges and */n or a-b/n
// steps. Weekdays are numbered 0 (Sunday) through 6 (Saturday); 7 is accepted as
// an alias for Sunday. Day of month and day of week follow standard cron semantics
// when at least one of them is restricted: a day matches when either field matches.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxHorizon bounds how far ahead a schedule is searched. An expression such as
// "0 0 29 2 *" can wait up to eight years for a leap day.
const maxHorizon = 10

// Field widths: minute, hour, day of month, month, day of week (0-6; 7 aliases 0).
var fieldBounds = [5][2]int{
	{0, 59},
	{0, 23},
	{1, 31},
	{1, 12},
	{0, 7},
}

// Schedule is a parsed cron expression with one bit set per allowed value.
type Schedule struct {
	minutes [60]bool
	hours   [24]bool
	doms    [32]bool // index 0 unused
	months  [13]bool // index 0 unused
	dows    [7]bool

	domRestricted bool
	dowRestricted bool
}

// Parse turns a five-field cron expression into a Schedule.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, errors.New("cron expression must have exactly five fields")
	}

	schedule := &Schedule{}
	parsers := []func(string) error{
		schedule.parseMinutes,
		schedule.parseHours,
		schedule.parseDayOfMonth,
		schedule.parseMonth,
		schedule.parseDayOfWeek,
	}
	for index, field := range fields {
		if err := parsers[index](field); err != nil {
			return nil, fmt.Errorf("field %d: %w", index+1, err)
		}
	}
	return schedule, nil
}

// NextAfter returns the earliest matching time strictly after after. The result uses
// the same time.Location as after.
func (s *Schedule) NextAfter(after time.Time) (time.Time, error) {
	loc := after.Location()
	candidate := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)

	for candidate.Year() <= after.Year()+maxHorizon {
		if !s.months[candidate.Month()] {
			candidate = time.Date(candidate.Year(), candidate.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !s.hours[candidate.Hour()] {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !s.minutes[candidate.Minute()] {
			candidate = candidate.Add(time.Minute)
			continue
		}
		return candidate, nil
	}
	return time.Time{}, errors.New("cron expression has no matching time within the supported horizon")
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.doms[t.Day()]
	dow := s.dows[int(t.Weekday())]
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

func parseField(raw string, low, high int, accept func(int), noteRestricted func()) error {
	if noteRestricted != nil {
		noteRestricted()
	}
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			return errors.New("empty list element")
		}
		step := 1
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			if slash == len(part)-1 {
				return errors.New("step value is missing")
			}
			value, err := strconv.Atoi(part[slash+1:])
			if err != nil || value <= 0 {
				return errors.New("step must be a positive integer")
			}
			step = value
			part = part[:slash]
		}

		start, end := low, high
		if part != "*" {
			if dash := strings.IndexByte(part, '-'); dash >= 0 {
				left, err := strconv.Atoi(part[:dash])
				if err != nil {
					return errors.New("range start must be a number")
				}
				right, err := strconv.Atoi(part[dash+1:])
				if err != nil {
					return errors.New("range end must be a number")
				}
				if left < low || right > high || left > right {
					return fmt.Errorf("range %d-%d is outside %d-%d", left, right, low, high)
				}
				start, end = left, right
			} else {
				value, err := strconv.Atoi(part)
				if err != nil {
					return errors.New("value must be a number, range or *")
				}
				if value < low || value > high {
					return fmt.Errorf("value %d is outside %d-%d", value, low, high)
				}
				start, end = value, value
			}
		}
		for value := start; value <= end; value += step {
			accept(value)
		}
	}
	return nil
}

func (s *Schedule) parseMinutes(field string) error {
	return parseField(field, fieldBounds[0][0], fieldBounds[0][1], func(v int) { s.minutes[v] = true }, nil)
}

func (s *Schedule) parseHours(field string) error {
	return parseField(field, fieldBounds[1][0], fieldBounds[1][1], func(v int) { s.hours[v] = true }, nil)
}

func (s *Schedule) parseDayOfMonth(field string) error {
	if field != "*" {
		s.domRestricted = true
	}
	return parseField(field, fieldBounds[2][0], fieldBounds[2][1], func(v int) { s.doms[v] = true }, nil)
}

func (s *Schedule) parseMonth(field string) error {
	return parseField(field, fieldBounds[3][0], fieldBounds[3][1], func(v int) { s.months[v] = true }, nil)
}

func (s *Schedule) parseDayOfWeek(field string) error {
	if field != "*" {
		s.dowRestricted = true
	}
	return parseField(field, fieldBounds[4][0], fieldBounds[4][1], func(v int) {
		if v == 7 {
			v = 0
		}
		s.dows[v] = true
	}, nil)
}
