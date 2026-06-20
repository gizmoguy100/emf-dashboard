package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseEventTimeUsesLondonDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}

	parsed, ok := parseEventTime("2026-06-01 12:30:00", loc)
	if !ok {
		t.Fatal("expected event time to parse")
	}
	if got := parsed.Format("15:04 MST -07:00"); got != "12:30 BST +01:00" {
		t.Fatalf("expected BST local time, got %s", got)
	}
}

func TestCountdownIncludesHoursAndMinutes(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	start := now.Add(2*time.Hour + 17*time.Minute)

	got := countdown(now, start)
	if !strings.Contains(got, "2 hours") || !strings.Contains(got, "17 mins") {
		t.Fatalf("expected hours and minutes countdown, got %q", got)
	}
}
