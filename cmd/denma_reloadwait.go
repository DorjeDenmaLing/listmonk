package main

// denma: a campaign that only waits (sending hours, daily limit) doesn't hold up its center's reload.

import "github.com/knadh/listmonk/internal/denmadaily"

// denmaCampaignsWaiting reports whether a center's campaigns can't send now: hours or a daily limit.
func denmaCampaignsWaiting(hoursOpen bool, limits ...denmadaily.Status) bool {
	if !hoursOpen {
		return true
	}
	for _, s := range limits {
		if s.Limit > 0 && s.Left <= s.Held {
			return true
		}
	}
	return false
}

// denmaSending reports whether a has a campaign sending now; one that only waits doesn't count.
func (a *App) denmaSending() bool {
	if !a.manager.HasRunningCampaigns() {
		return false
	}
	limits := []denmadaily.Status{denmaDaily.Status()}
	if c := denmaCenterCounter(a.ko); c != nil {
		limits = append(limits, c.Status())
	}
	return !denmaCampaignsWaiting(a.denmaSendingOpen(), limits...)
}
