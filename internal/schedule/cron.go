// Package schedule parses the five-field cron expressions used by the service
// and computes planned trigger times. All calculations operate on time.Time
// values as absolute instants; callers choose the zone they pass in and the
// service standardises on UTC at its boundaries.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidExpression is returned whenever a cron expression cannot be used
// to schedule trigger times, including a schedule that never fires.
var ErrInvalidExpression = errors.New("invalid cron expression")

const (
	minuteField = iota
	hourField
	domField
	monthField
	dowField
	fieldCount
)

var fieldBounds = [fieldCount][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 7},  // day of week (0 and 7 both mean Sunday)
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var weekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// Cron is a parsed five-field cron expression.
type Cron struct {
	minute []bool
	hour   []bool
	dom    []bool
	month  []bool
	dow    []bool

	domRestricted bool
	dowRestricted bool
}

// Parse validates a standard five-field cron expression:
//
//	┌───────── minute (0-59)
//	│ ┌─────── hour (0-23)
//	│ │ ┌───── day of month (1-31)
//	│ │ │ ┌─── month (1-12 or JAN-DEC)
//	│ │ │ │ ┌─ day of week (0-7 or SUN-SAT; 0 and 7 are Sunday)
//	│ │ │ │ │
//	* * * * *
//
// Each field accepts *, single values, comma lists, a-b ranges and */n or
// a-b/n steps. "?" is accepted in any field and matches every value.
func Parse(expr string) (*Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != fieldCount {
		return nil, fmt.Errorf("%w: expected 5 fields, got %d", ErrInvalidExpression, len(fields))
	}

	c := &Cron{}
	var err error
	if c.minute, err = parseField(fields[minuteField], 0, 59, nil); err != nil {
		return nil, err
	}
	if c.hour, err = parseField(fields[hourField], 0, 23, nil); err != nil {
		return nil, err
	}
	c.dom, c.domRestricted, err = parseFieldRestriction(fields[domField], 1, 31, nil)
	if err != nil {
		return nil, err
	}
	if c.month, err = parseField(fields[monthField], 1, 12, monthNames); err != nil {
		return nil, err
	}
	c.dow, c.dowRestricted, err = parseFieldRestriction(fields[dowField], 0, 7, weekdayNames)
	if err != nil {
		return nil, err
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	return c, nil
}

func parseField(raw string, min, max int, names map[string]int) ([]bool, error) {
	values, _, err := parseFieldRestriction(raw, min, max, names)
	return values, err
}

// parseFieldRestriction returns the match bitmap and whether the field was
// explicitly restricted (neither "*" nor "?"). The boolean drives cron's
// day-of-month / day-of-week union rule.
func parseFieldRestriction(raw string, min, max int, names map[string]int) ([]bool, bool, error) {
	if raw == "" {
		return nil, false, fmt.Errorf("%w: empty field", ErrInvalidExpression)
	}
	restricted := raw != "*" && raw != "?"
	values := make([]bool, max+1)
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			return nil, false, fmt.Errorf("%w: empty list item", ErrInvalidExpression)
		}
		step := 1
		hasStep := false
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			n, err := strconv.Atoi(part[slash+1:])
			if err != nil || n <= 0 {
				return nil, false, fmt.Errorf("%w: bad step in %q", ErrInvalidExpression, raw)
			}
			step = n
			hasStep = true
			part = part[:slash]
		}
		switch {
		case part == "*" || part == "?":
			for v := min; v <= max; v += step {
				values[v] = true
			}
		case strings.ContainsRune(part, '-'):
			bounds := strings.SplitN(part, "-", 2)
			lo, ok1 := parseToken(bounds[0], names)
			hi, ok2 := parseToken(bounds[1], names)
			if !ok1 || !ok2 || lo < min || hi > max || lo > hi {
				return nil, false, fmt.Errorf("%w: bad range in %q", ErrInvalidExpression, raw)
			}
			for v := lo; v <= hi; v += step {
				values[v] = true
			}
		default:
			v, ok := parseToken(part, names)
			if !ok || v < min || v > max {
				return nil, false, fmt.Errorf("%w: bad value in %q", ErrInvalidExpression, raw)
			}
			if !hasStep {
				values[v] = true
				break
			}
			for ; v <= max; v += step {
				values[v] = true
			}
		}
	}
	return values, restricted, nil
}

func parseToken(token string, names map[string]int) (int, bool) {
	if n, err := strconv.Atoi(token); err == nil {
		return n, true
	}
	if names != nil {
		if n, ok := names[strings.ToLower(token)]; ok {
			return n, true
		}
	}
	return 0, false
}

// Next returns the earliest matching instant strictly after after.
func (c *Cron) Next(after time.Time) (time.Time, error) {
	zone := after.Location()
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, zone).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	for !t.After(limit) {
		if !c.month[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, zone)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, zone)
			continue
		}
		if !c.hour[t.Hour()] {
			next := t.Add(time.Hour)
			t = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), 0, 0, 0, zone)
			continue
		}
		if !c.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: no trigger time within 5 years", ErrInvalidExpression)
}

func (c *Cron) dayMatches(t time.Time) bool {
	domMatch := c.dom[t.Day()]
	dowMatch := c.dow[int(t.Weekday())]
	switch {
	case !c.domRestricted && !c.dowRestricted:
		return true
	case !c.domRestricted:
		return dowMatch
	case !c.dowRestricted:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
