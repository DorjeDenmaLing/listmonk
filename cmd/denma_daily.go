package main

// denma: the hub's daily sending limit (Settings -> Performance,
// denma.daily_limit): the most e-mails every center together sends in any 24
// hours, as SES's sending quota counts them (a rolling 24 hours), so that
// campaigns never run into the quota. 0 is no limit.
//
// Every e-mail the process sends is counted (in the SMTP messenger, with the
// shared send rate, cmd/denma_sending.go), by the minute, in memory and in
// denma.daily_sends, so the count survives a restart or a deploy.
//
//   - Campaign messages wait when the limit is reached, and go on as the 24
//     hours roll on and the oldest sends drop out (internal/manager/denma.go);
//     a campaign paused or stopped meanwhile stops waiting.
//   - Automations send nothing while it's reached, and no more than what's
//     left of it; those due are sent on a later run (cmd/denma_automations.go).
//   - Opt-in confirmations, password resets, invites and notifications always
//     go, and count: so set the limit a little below the provider's quota.
//
// listmonk's sliding window (the same page) still works as before, in memory.

import (
	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/denmadaily"
)

// denmaDaily is the process's count, against the hub's limit.
var denmaDaily = denmadaily.New(lo)

// denmaInitDailySends loads and keeps the count. Called once, by
// denmaInitRegistry.
func denmaInitDailySends(db *sqlx.DB) error {
	return denmaDaily.Init(db)
}
