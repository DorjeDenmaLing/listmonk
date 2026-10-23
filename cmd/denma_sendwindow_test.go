package main

import (
	"testing"
	"time"
)

// Sending hours are checked when saved, and open only on their days,
// between their hours, in their time zone; off, they're always open.
func TestDenmaSendWindow(t *testing.T) {
	for _, c := range []struct {
		w   denmaSendWindow
		tz  string
		bad bool
	}{
		{denmaSendWindow{}, "", false},
		{denmaSendWindow{On: true, Days: []int{1}, From: "08:00", To: "21:00"}, "", true},
		{denmaSendWindow{On: true, Days: []int{1}, From: "08:00", To: "21:00"}, "Mars/Olympus", true},
		{denmaSendWindow{On: true, Days: []int{1}, From: "21:00", To: "08:00"}, "UTC", true},
		{denmaSendWindow{On: true, Days: []int{}, From: "08:00", To: "21:00"}, "UTC", true},
		{denmaSendWindow{On: true, Days: []int{7}, From: "08:00", To: "21:00"}, "UTC", true},
		{denmaSendWindow{On: true, Days: []int{1}, From: "8am", To: "21:00"}, "UTC", true},
		{denmaSendWindow{On: true, Days: []int{1}, From: "08:00", To: "21:00"}, "America/Halifax", false},
	} {
		if err := c.w.check(c.tz); (err != nil) != c.bad {
			t.Errorf("%+v in %q: error %v, want bad %v", c.w, c.tz, err, c.bad)
		}
	}

	w := denmaSendWindow{On: true, Days: []int{1, 2, 3, 4, 5}, From: "08:00", To: "21:00"}
	if err := w.check("America/Halifax"); err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for s, want := range map[string]bool{
		"2026-10-26T10:59:00Z": false, // Monday 07:59 in Halifax (UTC-3)
		"2026-10-26T12:00:00Z": true,  // Monday 09:00
		"2026-10-26T11:00:00Z": true,  // Monday 08:00, the start
		"2026-10-27T00:00:00Z": false, // Monday 21:00, the end
		"2026-10-25T15:00:00Z": false, // Sunday noon
		"2026-10-31T01:30:00Z": false, // Friday 22:30 in Halifax, though Saturday in UTC
		"2026-10-30T23:30:00Z": true,  // Friday 20:30
	} {
		if got := w.open(at(s)); got != want {
			t.Errorf("%s: open %v, want %v", s, got, want)
		}
	}
	if !(&denmaSendWindow{}).open(time.Now()) || !(*denmaSendWindow)(nil).open(time.Now()) {
		t.Error("hours that are off, or unset, are closed")
	}
}
