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
//   - Campaigns and automations stop short of the limit by its reserve
//     (denma.daily_reserve, a percentage of it, 5 to start with), so that
//     what they send never uses up the provider's quota.
//   - Campaign messages wait when they reach that, and go on as the 24 hours
//     roll on and the oldest sends drop out (internal/manager/denma.go); a
//     campaign paused or stopped meanwhile stops waiting.
//   - Automations send nothing while it's reached, and no more than what's
//     left; those due are sent on a later run (cmd/denma_automations.go).
//   - Opt-in confirmations, password resets, invites and notifications always
//     go, and count: the reserve is theirs.
//
// listmonk's sliding window isn't used with several centers; this replaces
// it. The first time the hub starts with it on, its rate becomes the daily
// limit (if the window is 24 hours and no limit is set) and it's turned off
// (denmaMoveSlidingWindow).

import (
	"fmt"
	"net/http"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/denmadaily"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaDaily is the process's count, against the hub's limit.
var denmaDaily = denmadaily.New(lo)

// denmaMaxDailyReserve is the most of the limit campaigns may be made to leave.
const denmaMaxDailyReserve = 50

// denmaInitDailySends loads and keeps the count. Called once, by
// denmaInitRegistry.
func denmaInitDailySends(db *sqlx.DB) error {
	return denmaDaily.Init(db)
}

// denmaCheckDailySettings refuses a daily limit or reserve out of range.
func denmaCheckDailySettings(set *models.Settings) error {
	if denmaHub == nil {
		return nil
	}
	if set.DenmaDailyLimit < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "The daily sending limit can't be less than 0.")
	}
	if set.DenmaDailyReserve < 0 || set.DenmaDailyReserve > denmaMaxDailyReserve {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("The share kept for confirmations and password resets has to be from 0 to %d%%.", denmaMaxDailyReserve))
	}
	return nil
}

// denmaMoveSlidingWindowSQL replaces listmonk's sliding window, if it's on,
// with the daily limit: a 24-hour window's rate becomes the limit, unless one
// is set, and the window is turned off. Run in the hub's schema at every
// start (denmaHubColumns); it changes nothing once the window is off.
const denmaMoveSlidingWindowSQL = `
UPDATE settings SET value = CASE key
	WHEN 'app.message_sliding_window' THEN 'false'::JSONB
	ELSE (SELECT value FROM settings WHERE key = 'app.message_sliding_window_rate') END
WHERE (SELECT value FROM settings WHERE key = 'app.message_sliding_window') = 'true'::JSONB
	AND (key = 'app.message_sliding_window'
		OR (key = 'denma.daily_limit' AND value = '0'::JSONB
			AND (SELECT value #>> '{}' FROM settings WHERE key = 'app.message_sliding_window_duration') IN ('24h', '1440m', '86400s')));`
