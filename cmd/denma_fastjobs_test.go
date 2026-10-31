package main

import (
	"testing"
	"time"
)

func TestDenmaFastJobs(t *testing.T) {
	for _, c := range []struct {
		list, center string
		want         bool
	}{
		{"", "halifax", false},
		{"", "", false},
		{"halifax,load-1", "halifax", true},
		{"halifax,load-1", "load-1", true},
		{"halifax,load-1", "load-10", false},
		{"halifax", "", false},
	} {
		if got := denmaFastJobs(c.list, c.center); got != c.want {
			t.Errorf("%q, %q: got %v", c.list, c.center, got)
		}
	}
}

func TestDenmaJobSchedules(t *testing.T) {
	at := func(m, s int) time.Time { return time.Date(2026, 10, 31, 12, m, s, 0, time.UTC) }
	for _, c := range []struct {
		name string
		got  time.Time
		want time.Time
	}{
		{"every minute, later this minute", denmaMinuteAt(30 * time.Second).Next(at(5, 10)), at(5, 30)},
		{"every minute, next minute", denmaMinuteAt(30 * time.Second).Next(at(5, 30)), at(6, 30)},
		{"every 10 seconds, later this 10", denmaFastAt(3 * time.Second).Next(at(5, 11)), at(5, 13)},
		{"every 10 seconds, the next 10", denmaFastAt(3 * time.Second).Next(at(5, 13)), at(5, 23)},
	} {
		if !c.got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}
