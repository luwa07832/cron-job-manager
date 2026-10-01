package cron

import (
	"testing"
	"time"
)

func utc(ts string) time.Time {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestNextBasic(t *testing.T) {
	s, err := Parse("*/15 * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := s.Next(utc("2026-01-01T10:07:00Z"))
	if !ok {
		t.Fatal("no next match")
	}
	if want := utc("2026-01-01T10:15:00Z"); !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextIsStrict(t *testing.T) {
	s, _ := Parse("0 12 * * *")
	got, ok := s.Next(utc("2026-01-01T12:00:00Z"))
	if !ok || !got.Equal(utc("2026-01-02T12:00:00Z")) {
		t.Fatalf("next = %v ok=%v", got, ok)
	}
}

func TestPrevAndLatest(t *testing.T) {
	s, _ := Parse("30 9 * * 1-5")
	got, ok := s.Prev(utc("2026-01-07T10:00:00Z")) // Wednesday
	if !ok || !got.Equal(utc("2026-01-07T09:30:00Z")) {
		t.Fatalf("prev = %v ok=%v", got, ok)
	}
	latest, ok := s.LatestAtOrBefore(utc("2026-01-07T09:30:30Z"))
	if !ok || !latest.Equal(utc("2026-01-07T09:30:00Z")) {
		t.Fatalf("latest = %v ok=%v", latest, ok)
	}
	none, ok := s.LatestAtOrBefore(utc("2026-01-04T09:30:00Z")) // Sunday
	if !ok || !none.Equal(utc("2026-01-02T09:30:00Z")) {
		t.Fatalf("latest weekend = %v ok=%v", none, ok)
	}
}

func TestNames(t *testing.T) {
	if _, err := Parse("0 0 1 jan-jun mon"); err != nil {
		t.Fatalf("named fields rejected: %v", err)
	}
	if _, err := Parse("0 0 * * sun"); err != nil {
		t.Fatalf("named dow rejected: %v", err)
	}
}

func TestDomDowUnion(t *testing.T) {
	// 13th of any month or any Friday
	s, err := Parse("0 0 13 * fri")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := s.Next(utc("2026-01-01T00:00:00Z"))
	if !ok {
		t.Fatal("no match")
	}
	// 2026-01-02 is Friday, precedes the 13th.
	if want := utc("2026-01-02T00:00:00Z"); !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestInvalidExpressions(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",
		"60 * * * *",
		"* 24 * * *",
		"0 0 0 * *",
		"0 0 * 13 *",
		"*/0 * * * *",
		"a b c d e",
		"0 0 32 * *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestNeverMatchingSchedule(t *testing.T) {
	// 31st of February never occurs; search horizon yields no match.
	s, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := s.Next(utc("2026-01-01T00:00:00Z")); ok {
		t.Fatal("unexpected match for Feb 31")
	}
}

func TestBetweenInclusiveAndOrdered(t *testing.T) {
	s, _ := Parse("0 * * * *")
	matches := s.Between(utc("2026-01-01T10:00:00Z"), utc("2026-01-01T13:00:00Z"))
	if len(matches) != 4 {
		t.Fatalf("len = %d, want 4: %v", len(matches), matches)
	}
	if !matches[0].Equal(utc("2026-01-01T10:00:00Z")) ||
		!matches[3].Equal(utc("2026-01-01T13:00:00Z")) {
		t.Fatalf("bounds wrong: %v", matches)
	}
}

func TestBetweenOffGridStart(t *testing.T) {
	s, _ := Parse("0 0 * * *")
	matches := s.Between(utc("2026-01-01T12:00:00Z"), utc("2026-01-03T00:00:00Z"))
	if len(matches) != 2 {
		t.Fatalf("len = %d, want 2: %v", len(matches), matches)
	}
}
