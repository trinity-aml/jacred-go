package server

import (
	"strings"
	"testing"
	"time"
)

func TestFormatLocalDateTimeIsUTC(t *testing.T) {
	// 12:00 UTC stays as 12:00 in the formatted output regardless of TZ —
	// the previous code shifted to +0200 producing 14:00 here.
	in := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	got := formatLocalDateTime(in)
	want := "2026-05-09T12:00:00Z"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFormatLocalDateTimeZSuffix(t *testing.T) {
	// Sanity: the Z suffix is what tells JS new Date() to parse as UTC.
	got := formatLocalDateTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(got, "Z") {
		t.Fatalf("missing Z suffix: %q", got)
	}
}

func TestFormatLocalDateTimeIgnoresInputZone(t *testing.T) {
	// Same instant, two zones — output must be identical.
	utc := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	moscow := utc.In(time.FixedZone("MSK", 3*3600))
	if formatLocalDateTime(utc) != formatLocalDateTime(moscow) {
		t.Fatalf("zone-shifted inputs produced different formatted output")
	}
}
