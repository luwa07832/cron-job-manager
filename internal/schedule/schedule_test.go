package schedule

import (
	"testing"
	"time"
)

func atUTC(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestParseRejectsBadFieldCount(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "  "} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestParseRejectsOutOfBounds(t *testing.T) {
	for _, expr := range []string{
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 7",
		"*/0 * * * *",
		"5/* * * * *",
		"1- * * * *",
		"* * * * 1-8",
		"1,2, * * * *",
		"a * * * *",
		"1--2 * * * *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestParseAcceptsShapes(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"0 0 1 1 0",
		"5,10,15 9-17 */2 1-12/3 0,6",
		"0-59/15 * * * *",
		"0 0 * * 6",
	} {
		if _, err := Parse(expr); err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	spec, err := Parse("30 10 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// Exactly at 10:30 must move to the next day.
	from := atUTC(2026, time.October, 2, 10, 30)
	got := spec.Next(from, time.UTC)
	want := atUTC(2026, time.October, 3, 10, 30)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
	if !got.After(from) {
		t.Fatalf("Next %s must be after %s", got, from)
	}
}

func TestNextRangesListsAndSteps(t *testing.T) {
	spec, err := Parse("15,45 8-10/2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Next(atUTC(2026, time.October, 2, 9, 50), time.UTC)
	want := atUTC(2026, time.October, 2, 10, 15)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestNextRollsMonthAndDayOfMonth(t *testing.T) {
	spec, err := Parse("0 0 1 * *")
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Next(atUTC(2026, time.January, 15, 12, 0), time.UTC)
	want := atUTC(2026, time.February, 1, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestNextRestrictsMonth(t *testing.T) {
	spec, err := Parse("0 0 1 12 *")
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Next(atUTC(2026, time.January, 1, 0, 0), time.UTC)
	want := atUTC(2026, time.December, 1, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestDayOfWeekRestriction(t *testing.T) {
	spec, err := Parse("0 9 * * 1")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-02 is a Friday.
	got := spec.Next(atUTC(2026, time.October, 2, 10, 0), time.UTC)
	want := atUTC(2026, time.October, 5, 9, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestDayOfMonthAndWeekdayAreOred(t *testing.T) {
	spec, err := Parse("0 0 13 * 5")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-02 Friday: next match is the same day (weekday match).
	got := spec.Next(atUTC(2026, time.October, 1, 23, 59), time.UTC)
	want := atUTC(2026, time.October, 2, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}

	// From Saturday Oct 10 (after the Oct 9 Friday) the next match is the
	// 13th via the day-of-month field.
	got = spec.Next(atUTC(2026, time.October, 10, 0, 0), time.UTC)
	want = atUTC(2026, time.October, 13, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestNextHonorsTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-02 08:30 Shanghai -> 09:00 Shanghai, which is 01:00 UTC.
	from := time.Date(2026, time.October, 2, 8, 30, 0, 0, loc)
	got := spec.Next(from, loc).UTC()
	want := atUTC(2026, time.October, 2, 1, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestUnsatisfiableScheduleReturnsZero(t *testing.T) {
	spec, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Next(atUTC(2026, time.January, 1, 0, 0), time.UTC); !got.IsZero() {
		t.Fatalf("Next = %s, want zero", got)
	}
}

func TestLeapDayScheduleFires(t *testing.T) {
	spec, err := Parse("0 0 29 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Next(atUTC(2025, time.January, 1, 0, 0), time.UTC)
	want := atUTC(2028, time.February, 29, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}
