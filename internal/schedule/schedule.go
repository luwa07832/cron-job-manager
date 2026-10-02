// Package schedule parses five-field cron expressions and computes the next
// firing instant in an arbitrary IANA time zone.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// field bounds for the five cron positions: minute, hour, day of month, month,
// day of week. Weekdays follow Go's numbering with Sunday = 0.
const (
	minuteMax = 59
	hourMax   = 23
	domMax    = 31
	monthMax  = 12
	dowMax    = 6

	// maxYears bounds the forward search so an unsatisfiable expression
	// (for example the 30th of February) is reported instead of looping.
	maxYears = 28
)

// Spec is a parsed five-field cron expression.
type Spec struct {
	minute     map[int]bool
	hour       map[int]bool
	dayOfMonth map[int]bool
	month      map[int]bool
	dayOfWeek  map[int]bool

	domWild bool
	dowWild bool
}

// Parse interprets a five-field cron expression:
//
//	minute hour day-of-month month day-of-week
//
// Every field accepts "*", a number, a comma-separated list, "a-b" ranges and
// "value/step" steps applied either to "*" or to a range.
func Parse(expr string) (Spec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Spec{}, fmt.Errorf("schedule: expected 5 fields, got %d", len(fields))
	}

	minute, err := parseField(fields[0], 0, minuteMax)
	if err != nil {
		return Spec{}, err
	}
	hour, err := parseField(fields[1], 0, hourMax)
	if err != nil {
		return Spec{}, err
	}
	dayOfMonth, err := parseField(fields[2], 1, domMax)
	if err != nil {
		return Spec{}, err
	}
	month, err := parseField(fields[3], 1, monthMax)
	if err != nil {
		return Spec{}, err
	}
	dayOfWeek, err := parseField(fields[4], 0, dowMax)
	if err != nil {
		return Spec{}, err
	}

	return Spec{
		minute:     minute.values,
		hour:       hour.values,
		dayOfMonth: dayOfMonth.values,
		month:      month.values,
		dayOfWeek:  dayOfWeek.values,
		domWild:    dayOfMonth.wild,
		dowWild:    dayOfWeek.wild,
	}, nil
}

type parsedField struct {
	values map[int]bool
	wild   bool
}

func parseField(raw string, min, max int) (parsedField, error) {
	values := make(map[int]bool)
	wild := false
	for _, term := range strings.Split(raw, ",") {
		if term == "" {
			return parsedField{}, fmt.Errorf("schedule: empty list term")
		}

		step := 0
		rangePart := term
		if slash := strings.IndexByte(term, '/'); slash >= 0 {
			rangePart = term[:slash]
			if rangePart == "" || slash == len(term)-1 {
				return parsedField{}, fmt.Errorf("schedule: bad step %q", term)
			}
			stepText := term[slash+1:]
			parsed, err := strconv.Atoi(stepText)
			if err != nil || parsed <= 0 || strings.HasPrefix(stepText, "+") {
				return parsedField{}, fmt.Errorf("schedule: bad step %q", term)
			}
			step = parsed
		}

		var lo, hi int
		switch rangePart {
		case "*":
			wild = true
			lo, hi = min, max
		default:
			if dash := strings.IndexByte(rangePart, '-'); dash >= 0 {
				loText := rangePart[:dash]
				hiText := rangePart[dash+1:]
				lo = parseNumber(loText)
				hi = parseNumber(hiText)
				if lo == -1 || hi == -1 {
					return parsedField{}, fmt.Errorf("schedule: bad range %q", rangePart)
				}
			} else {
				lo = parseNumber(rangePart)
				if lo == -1 {
					return parsedField{}, fmt.Errorf("schedule: bad value %q", rangePart)
				}
				hi = lo
				if step > 0 {
					hi = max
				}
			}
		}

		if lo < min || hi > max || lo > hi {
			return parsedField{}, fmt.Errorf("schedule: value out of bounds %q", term)
		}
		if step == 0 {
			step = 1
		}
		for value := lo; value <= hi; value += step {
			values[value] = true
		}
	}
	return parsedField{values: values, wild: wild}, nil
}

// parseNumber accepts only plain non-negative decimal integers and rejects
// things such as "*", "5x" or signed numbers inside ranges.
func parseNumber(text string) int {
	if text == "" {
		return -1
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 0 {
		return -1
	}
	return value
}

// Next returns the first instant strictly after from that matches the
// expression, interpreted in loc. The zero time is returned when the
// expression can never fire within the search horizon.
func (s Spec) Next(from time.Time, loc *time.Location) time.Time {
	local := from.In(loc)
	candidate := time.Date(
		local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), 0, 0, loc,
	).Add(time.Minute)
	limit := time.Date(from.Year()+maxYears+1, 1, 1, 0, 0, 0, 0, loc)

	for candidate.Before(limit) {
		if !s.month[int(candidate.Month())] {
			candidate = time.Date(candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.hour[candidate.Hour()] {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !s.minute[candidate.Minute()] {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour(), candidate.Minute()+1, 0, 0, loc)
			continue
		}
		return candidate
	}
	return time.Time{}
}

// Through returns every instant matching the expression in the closed
// interval [from, to], computed in loc and returned in UTC ascending order,
// together with the first matching instant strictly after to. The follow-up
// instant is the zero time when no match exists within the search horizon.
// from must itself be a matching instant, which holds for every stored
// schedule cursor.
func (s Spec) Through(from, to time.Time, loc *time.Location) ([]time.Time, time.Time) {
	if from.After(to) {
		return nil, s.Next(to, loc)
	}
	matches := []time.Time{from.UTC()}
	cursor := from
	for {
		next := s.Next(cursor, loc)
		if next.IsZero() || next.After(to) {
			return matches, next
		}
		matches = append(matches, next.UTC())
		cursor = next
	}
}

// dayMatches applies standard cron semantics: when both day-of-month and
// day-of-week are restricted they are ORed together; "*" on either side means
// the other field alone decides.
func (s Spec) dayMatches(t time.Time) bool {
	domMatch := s.dayOfMonth[t.Day()]
	dowMatch := s.dayOfWeek[int(t.Weekday())]
	switch {
	case s.domWild && s.dowWild:
		return true
	case s.domWild:
		return dowMatch
	case s.dowWild:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
