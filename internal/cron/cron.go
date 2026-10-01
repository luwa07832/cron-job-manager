// Package cron parses standard five-field schedules and computes fire instants.
//
// All schedules are interpreted at minute granularity in UTC, so results never
// depend on the local machine zone.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxSearchBounds limits how far ahead or behind a schedule is searched. A
// syntactically valid schedule that never matches inside this window (for
// example "0 0 29 2 *") is rejected as unusable.
const maxSearchBounds = 5 * 366 * 24 * time.Hour

// Schedule is a parsed five-field cron expression (minute hour dom month dow).
type Schedule struct {
	expr   string
	minute uint64
	hour   uint64
	dom    uint64
	month  uint64
	dow    uint64
	// restrictedDom/restrictedDow mark fields that constrain the match instead
	// of covering every value. Only a bare "*" is unrestricted, which follows
	// the classic cron AND/OR rule.
	restrictedDom bool
	restrictedDow bool
}

const (
	bitMinute = 60
	bitHour   = 24
	bitDom    = 31
	bitMonth  = 12
	bitDow    = 7
)

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// Parse validates a five-field cron expression and returns its Schedule.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q must contain exactly five fields", expr)
	}
	s := &Schedule{expr: expr}
	var err error
	if s.minute, err = parseField(fields[0], 0, bitMinute-1, nil); err != nil {
		return nil, fmt.Errorf("invalid minute field: %w", err)
	}
	if s.hour, err = parseField(fields[1], 0, bitHour-1, nil); err != nil {
		return nil, fmt.Errorf("invalid hour field: %w", err)
	}
	if s.dom, s.restrictedDom, err = parseRestricted(fields[2], 1, bitDom, nil); err != nil {
		return nil, fmt.Errorf("invalid day-of-month field: %w", err)
	}
	if s.month, err = parseField(fields[3], 1, bitMonth, monthNames); err != nil {
		return nil, fmt.Errorf("invalid month field: %w", err)
	}
	if s.dow, s.restrictedDow, err = parseRestricted(fields[4], 0, bitDow-1, dowNames); err != nil {
		return nil, fmt.Errorf("invalid day-of-week field: %w", err)
	}
	return s, nil
}

// Expression returns the original expression text.
func (s *Schedule) Expression() string { return s.expr }

func parseRestricted(field string, min, max int, names map[string]int) (uint64, bool, error) {
	bits, err := parseField(field, min, max, names)
	if err != nil {
		return 0, false, err
	}
	return bits, field != "*", nil
}

func parseField(field string, min, max int, names map[string]int) (uint64, error) {
	if field == "" {
		return 0, fmt.Errorf("empty field")
	}
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list element")
		}
		step := 1
		rangePart := part
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			stepText := part[slash+1:]
			if stepText == "" || strings.ContainsRune(stepText, '/') {
				return 0, fmt.Errorf("invalid step in %q", part)
			}
			value, err := strconv.Atoi(stepText)
			if err != nil || value <= 0 {
				return 0, fmt.Errorf("invalid step in %q", part)
			}
			step = value
			rangePart = part[:slash]
		}

		low, high, err := parseRange(rangePart, min, max, names)
		if err != nil {
			return 0, err
		}
		if low < min || high > max || low > high {
			return 0, fmt.Errorf("value out of range [%d,%d] in %q", min, max, part)
		}
		for value := low; value <= high; value += step {
			bits |= 1 << uint(value-min)
		}
	}
	return bits, nil
}

func parseRange(text string, min, max int, names map[string]int) (int, int, error) {
	if text == "*" {
		return min, max, nil
	}
	resolve := func(token string) (int, error) {
		if value, err := strconv.Atoi(token); err == nil {
			return value, nil
		}
		if names != nil {
			if value, ok := names[strings.ToLower(token)]; ok {
				return value, nil
			}
		}
		return 0, fmt.Errorf("invalid value %q", token)
	}
	if dash := strings.IndexByte(text, '-'); dash >= 0 {
		low, err := resolve(text[:dash])
		if err != nil {
			return 0, 0, err
		}
		high, err := resolve(text[dash+1:])
		if err != nil {
			return 0, 0, err
		}
		return low, high, nil
	}
	value, err := resolve(text)
	if err != nil {
		return 0, 0, err
	}
	return value, value, nil
}

func bitSet(bits uint64, value, min int) bool {
	return bits&(1<<uint(value-min)) != 0
}

func (s *Schedule) matchesInstant(t time.Time) bool {
	return bitSet(s.month, int(t.Month()), 1) &&
		bitSet(s.minute, t.Minute(), 0) &&
		bitSet(s.hour, t.Hour(), 0) &&
		s.matchesDay(t)
}

// matchesDay implements the classic cron rule: when both day fields are
// restricted the union is used; when only one is restricted it alone decides.
func (s *Schedule) matchesDay(t time.Time) bool {
	domMatch := bitSet(s.dom, t.Day(), 1)
	dowMatch := bitSet(s.dow, int(t.Weekday()), 0)
	switch {
	case s.restrictedDom && s.restrictedDow:
		return domMatch || dowMatch
	case s.restrictedDom:
		return domMatch
	case s.restrictedDow:
		return dowMatch
	default:
		return true
	}
}

// Next returns the earliest match strictly after after; ok is false when no
// match exists inside the bounded search horizon.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	candidate := after.Truncate(time.Minute).In(time.UTC).Add(time.Minute)
	deadline := after.In(time.UTC).Add(maxSearchBounds)
	for !candidate.After(deadline) {
		if s.matchesInstant(candidate) {
			return candidate, true
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, false
}

// Prev returns the latest match strictly before before; ok is false when no
// match exists inside the bounded search horizon.
func (s *Schedule) Prev(before time.Time) (time.Time, bool) {
	candidate := before.Truncate(time.Minute).In(time.UTC).Add(-time.Minute)
	deadline := before.In(time.UTC).Add(-maxSearchBounds)
	for !candidate.Before(deadline) {
		if s.matchesInstant(candidate) {
			return candidate, true
		}
		candidate = candidate.Add(-time.Minute)
	}
	return time.Time{}, false
}

// Between enumerates every match m with from <= m <= to in chronological order.
// Both endpoints are inclusive. Ranges must stay within the search bounds.
func (s *Schedule) Between(from, to time.Time) []time.Time {
	start := from.Truncate(time.Minute).In(time.UTC)
	if start.Before(from.In(time.UTC)) {
		start = start.Add(time.Minute)
	}
	end := to.In(time.UTC)
	var matches []time.Time
	if start.After(end) {
		return matches
	}
	for candidate := start; !candidate.After(end); {
		if s.matchesInstant(candidate) {
			matches = append(matches, candidate)
		}
		next, ok := s.Next(candidate)
		if !ok || next.After(end) {
			break
		}
		candidate = next
	}
	return matches
}

// LatestAtOrBefore returns the latest match m with m <= t; ok is false when no
// match exists inside the bounded search horizon.
func (s *Schedule) LatestAtOrBefore(t time.Time) (time.Time, bool) {
	candidate := t.Truncate(time.Minute).In(time.UTC)
	deadline := t.In(time.UTC).Add(-maxSearchBounds)
	for !candidate.Before(deadline) {
		if s.matchesInstant(candidate) {
			return candidate, true
		}
		candidate = candidate.Add(-time.Minute)
	}
	return time.Time{}, false
}
