package cron

import (
	"testing"
	"time"
)

func at(t *testing.T, value, locName string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(locName)
	if err != nil {
		t.Fatalf("load location %s: %v", locName, err)
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatalf("parse %s: %v", value, err)
	}
	return parsed
}

func wantAt(t *testing.T, got time.Time, value, locName string) {
	t.Helper()
	want := at(t, value, locName)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextAfterBasicFields(t *testing.T) {
	schedule, err := Parse("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-10-02 10:07", "UTC")) // Friday
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-10-02 10:15", "UTC")
}

func TestNextAfterIsStrictlyAfter(t *testing.T) {
	schedule, err := Parse("30 0 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-10-02 00:30", "UTC"))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-10-03 00:30", "UTC")
}

func TestNextAfterRollsAcrossDaysMonthsAndYears(t *testing.T) {
	schedule, err := Parse("0 22 1 * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-10-02 23:00", "UTC"))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-11-01 22:00", "UTC")

	yearly, err := Parse("0 0 1 1 *")
	if err != nil {
		t.Fatalf("parse yearly: %v", err)
	}
	got, err = yearly.NextAfter(at(t, "2026-12-31 23:59", "UTC"))
	if err != nil {
		t.Fatalf("next yearly: %v", err)
	}
	wantAt(t, got, "2027-01-01 00:00", "UTC")
}

func TestNextAfterLeapDay(t *testing.T) {
	schedule, err := Parse("0 0 29 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-03-01 00:00", "UTC"))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2028-02-29 00:00", "UTC")
}

func TestDayOfWeekUsesORSemanticsWhenRestricted(t *testing.T) {
	schedule, err := Parse("0 12 13 * 5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Friday 2026-11-13 matches via day of week even though the 13th also exists.
	got, err := schedule.NextAfter(at(t, "2026-11-01 00:00", "UTC"))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-11-06 12:00", "UTC") // first Friday
}

func TestSundaySevenAliasesZero(t *testing.T) {
	schedule, err := Parse("0 0 * * 7")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-10-02 12:00", "UTC")) // Friday
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-10-04 00:00", "UTC")
}

func TestNextAfterRespectsLocation(t *testing.T) {
	schedule, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := schedule.NextAfter(at(t, "2026-10-02 08:00", "Asia/Shanghai"))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	wantAt(t, got, "2026-10-02 09:00", "Asia/Shanghai")
	if offset := got.UTC().Format("-07:00"); offset != "+00:00" {
		t.Fatalf("utc form = %v", got.UTC())
	}
	if got.UTC().Format("15:04") != "01:00" {
		t.Fatalf("utc hour = %s, want 01:00", got.UTC().Format("15:04"))
	}
}

func TestParseRejectsInvalidExpressions(t *testing.T) {
	invalid := []string{
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 8",
		"5-3 * * * *",
		"*/0 * * * *",
		"*/x * * * *",
		"1,,2 * * * *",
		"a * * * *",
		"1- * * * *",
		"*/5/2 * * * *",
	}
	for _, expr := range invalid {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestImpossibleScheduleHasNoNextWithinHorizon(t *testing.T) {
	schedule, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := schedule.NextAfter(at(t, "2026-01-01 00:00", "UTC")); err == nil {
		t.Fatalf("NextAfter succeeded for Feb 30, want error")
	}
}
