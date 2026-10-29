package main

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
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

// The next opening of the hours: the same day before they start, the next
// day they're on, a week on from the last of them, in the center's time zone.
func TestDenmaSendWindowNext(t *testing.T) {
	w := denmaSendWindow{On: true, Days: []int{1, 2, 3, 4, 5}, From: "08:00", To: "21:00"}
	if err := w.check("America/Halifax"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ at, want string }{
		{"2026-10-26T12:00:00Z", "2026-10-26T12:00:00Z"}, // Monday 09:00, open
		{"2026-10-26T10:00:00Z", "2026-10-26T11:00:00Z"}, // Monday 07:00 -> 08:00
		{"2026-10-27T00:00:00Z", "2026-10-27T11:00:00Z"}, // Monday 21:00, the end -> Tuesday 08:00
		{"2026-10-31T01:30:00Z", "2026-11-02T12:00:00Z"}, // Friday 22:30 -> Monday 08:00 (the clocks went back on Nov 1)
		{"2026-10-25T15:00:00Z", "2026-10-26T11:00:00Z"}, // Sunday noon -> Monday 08:00
		{"2026-10-26T11:00:00Z", "2026-10-26T11:00:00Z"}, // Monday 08:00, the start
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := w.next(at); !got.Equal(want) {
			t.Errorf("%s: next %s, want %s", c.at, got.UTC().Format(time.RFC3339), c.want)
		}
	}
	one := denmaSendWindow{On: true, Days: []int{3}, From: "09:30", To: "10:00"}
	if err := one.check("UTC"); err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, "2026-10-28T10:00:00Z") // Wednesday, just after the hours
	if got, want := one.next(at).Format(time.RFC3339), "2026-11-04T09:30:00Z"; got != want {
		t.Errorf("a week on: %s, want %s", got, want)
	}
	if got := w.describe(); got != "Mon-Fri 08:00-21:00, America/Halifax" {
		t.Errorf("describe: %q", got)
	}
	if got := (&denmaSendWindow{Days: []int{0, 2, 3, 6}, From: "08:00", To: "09:00", loc: time.UTC}).describe(); got != "Sun, Tue, Wed, Sat 08:00-09:00, UTC" {
		t.Errorf("describe: %q", got)
	}
}

// goesSQL gives what next does, across a change of clocks, for every
// 37 minutes of five weeks. Needs the test database (go-wt.sh).
func TestDenmaSendWindowSQL(t *testing.T) {
	host, name := os.Getenv("LISTMONK_db__host"), os.Getenv("LISTMONK_db__database")
	if host == "" || name == "" {
		t.Skip("no test database")
	}
	db, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%s user=listmonk password=listmonk dbname=%s sslmode=disable",
		host, os.Getenv("LISTMONK_db__port"), name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, w := range []denmaSendWindow{
		{On: true, Days: []int{1, 2, 3, 4, 5}, From: "08:00", To: "21:00"},
		{On: true, Days: []int{0, 6}, From: "09:30", To: "10:15"},
		{On: true, Days: []int{3}, From: "00:00", To: "23:59"},
	} {
		if err := w.check("America/Halifax"); err != nil {
			t.Fatal(err)
		}
		q := "SELECT " + w.goesSQL("$1::TIMESTAMPTZ")
		start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		for at := start; at.Before(start.AddDate(0, 0, 35)); at = at.Add(37 * time.Minute) {
			var got time.Time
			if err := db.QueryRow(q, at).Scan(&got); err != nil {
				t.Fatalf("%v: %v\n%s", w.Days, err, q)
			}
			if want := w.next(at); !got.Equal(want) {
				t.Fatalf("%v %s: SQL %s, next %s", w.Days, at.Format(time.RFC3339), got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
			}
		}
	}
	if (*denmaSendWindow)(nil).goesSQL("x") != "x" {
		t.Error("hours that are off change nothing")
	}
}
