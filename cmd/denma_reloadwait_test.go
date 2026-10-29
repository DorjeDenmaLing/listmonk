package main

import (
	"testing"

	"github.com/knadh/listmonk/internal/denmadaily"
)

// Campaigns wait outside the sending hours, or with a daily limit reached
// (no limit isn't one); otherwise they're sending.
func TestDenmaCampaignsWaiting(t *testing.T) {
	for _, c := range []struct {
		open   bool
		limits []denmadaily.Status
		want   bool
	}{
		{true, nil, false},
		{false, nil, true},
		{true, []denmadaily.Status{{}}, false},
		{true, []denmadaily.Status{{Limit: 100, Left: 5}}, false},
		{true, []denmadaily.Status{{Limit: 100, Left: 0}}, true},
		{true, []denmadaily.Status{{Limit: 100, Left: 5}, {Limit: 10, Left: 0}}, true},
		{false, []denmadaily.Status{{Limit: 100, Left: 5}}, true},
	} {
		if got := denmaCampaignsWaiting(c.open, c.limits...); got != c.want {
			t.Errorf("open %v, limits %+v: %v, want %v", c.open, c.limits, got, c.want)
		}
	}
}
