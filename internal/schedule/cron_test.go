package schedule

import (
	"errors"
	"testing"
	"time"
)

func utc(y int, month time.Month, d, h, m int) time.Time {
	return time.Date(y, month, d, h, m, 0, 0, time.UTC)
}

func TestNextBasicFields(t *testing.T) {
	tests := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{"every minute", "* * * * *", utc(2026, 10, 1, 10, 0), utc(2026, 10, 1, 10, 1)},
		{"fixed minute", "30 * * * *", utc(2026, 10, 1, 10, 45), utc(2026, 10, 1, 11, 30)},
		{"fixed hour", "0 8 * * *", utc(2026, 10, 1, 9, 0), utc(2026, 10, 2, 8, 0)},
		{"list", "15,45 8 * * *", utc(2026, 10, 1, 8, 20), utc(2026, 10, 1, 8, 45)},
		{"range step", "0-30/10 9 * * *", utc(2026, 10, 1, 9, 11), utc(2026, 10, 1, 9, 20)},
		{"star step", "*/15 * * * *", utc(2026, 10, 1, 10, 7), utc(2026, 10, 1, 10, 15)},
		{"month names", "0 0 1 jan,jul *", utc(2026, 2, 1, 0, 0), utc(2026, 7, 1, 0, 0)},
		{"weekday names", "0 9 * * mon-fri", utc(2026, 10, 2, 10, 0), utc(2026, 10, 5, 9, 0)},
		{"sunday seven", "0 9 * * 7", utc(2026, 10, 1, 10, 0), utc(2026, 10, 4, 9, 0)},
		{"sunday zero", "0 9 * * 0", utc(2026, 10, 1, 10, 0), utc(2026, 10, 4, 9, 0)},
		{"day of month", "0 0 15 * *", utc(2026, 10, 20, 0, 0), utc(2026, 11, 15, 0, 0)},
		{"rolls year", "0 0 1 1 *", utc(2026, 6, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cron, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := cron.Next(tt.from)
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("next = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	cron, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := cron.Next(utc(2026, 10, 1, 9, 0))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if want := utc(2026, 10, 2, 9, 0); !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextRespectsZoneButStaysAbsolute(t *testing.T) {
	cron, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	loc := time.FixedZone("UTC+8", 8*60*60)
	from := time.Date(2026, 10, 1, 10, 0, 0, 0, loc)
	got, err := cron.Next(from)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	want := time.Date(2026, 10, 2, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestInvalidExpressions(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 32 * *", "* * * 13 *", "* * * * 8", "a * * * *",
		"5-2 * * * *", "*/0 * * * *", "*/x * * * *", "1,,2 * * * *",
	} {
		t.Run(expr, func(t *testing.T) {
			if _, err := Parse(expr); !errors.Is(err, ErrInvalidExpression) {
				t.Fatalf("Parse(%q) err = %v, want ErrInvalidExpression", expr, err)
			}
		})
	}
}

func TestNextSequenceWalksForward(t *testing.T) {
	cron, err := Parse("0 0 29 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := cron.Next(utc(2026, 1, 1, 0, 0))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if want := utc(2028, 2, 29, 0, 0); !got.Equal(want) {
		t.Fatalf("next leap day = %s, want %s", got, want)
	}
}

func TestImpossibleScheduleRejectedByFiveYearWindow(t *testing.T) {
	// February 31st never exists, so no trigger time can be computed.
	cron, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := cron.Next(utc(2026, 1, 1, 0, 0)); !errors.Is(err, ErrInvalidExpression) {
		t.Fatalf("next err = %v, want ErrInvalidExpression", err)
	}
}
