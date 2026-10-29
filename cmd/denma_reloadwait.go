package main

// denma: a campaign that only waits (for its center's sending hours, or a
// daily limit) doesn't hold up its center's reload: the reload stops it
// (denmaRetire) and it goes on in the new App from where it was.

import "github.com/knadh/listmonk/internal/denmadaily"

// denmaCampaignsWaiting reports whether a center's campaigns can't send
// now: outside its sending hours, or a daily limit (the hub's, the
// center's) reached, or what's left of it kept for others.
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

// denmaSending reports whether a has a campaign sending now. One that only
// waits for the sending hours or the daily limit doesn't count: a reload
// stops it, and it goes on in the new App from where it was.
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
