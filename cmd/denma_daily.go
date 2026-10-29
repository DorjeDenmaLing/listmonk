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
//     roll on and the oldest sends drop out (internal/manager/denma.go), in
//     order, one center's after another's in turn; a campaign paused or
//     stopped meanwhile stops waiting, and gives its count back. Room a
//     transactional e-mail or an automation was refused is left for it.
//   - Automations send nothing while it's reached, and no more than what's
//     left; those due are sent on a later run (cmd/denma_automations.go).
//   - Opt-in confirmations, password resets, invites and notifications always
//     go, and count: the reserve is theirs.
//
// listmonk's sliding window isn't used with several centers; this replaces
// it. The first time the hub starts with it on, its rate becomes the daily
// limit (if the window is 24 hours and no limit is set) and it's turned off
// (denmaMoveSlidingWindow).
//
// Each center also has a daily cap of its own (denma.daily_cap, 0 for none),
// which only a superadmin sets, on the center's Config -> General page: the
// most e-mails it sends in any 24 hours, counted in denma.center_sends. It
// counts what the center's campaign manager sends: campaigns, automations and
// the sending API's e-mails (cmd/denma_api.go). At the cap, its campaign
// messages wait (internal/manager/denma.go), its automations send nothing,
// and the API refuses e-mails (429). Its opt-in confirmations, password
// resets, invites and notifications always go, and aren't counted.

import (
	"fmt"
	"net/http"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/denmadaily"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaDaily is the process's count, against the hub's limit.
var denmaDaily = denmadaily.New(lo)

// denmaCenterSends is each center's count, against its own daily cap.
var denmaCenterSends = denmadaily.NewCounters(lo)

// denmaMaxDailyCap is the highest daily cap a center can have.
const denmaMaxDailyCap = 1_000_000

// denmaMaxDailyReserve is the most of the limit campaigns may be made to leave.
const denmaMaxDailyReserve = 50

// denmaInitDailySends loads and keeps the count. Called once, by
// denmaInitRegistry.
func denmaInitDailySends(db *sqlx.DB) error {
	if err := denmaDaily.Init(db); err != nil {
		return err
	}
	return denmaCenterSends.Init(db)
}

// denmaCenterCounter is a center's count; nil for the hub, or a single
// install.
func denmaCenterCounter(ko *koanf.Koanf) *denmadaily.Counter {
	center := ko.String("denma.center")
	if !ko.Bool("denma.multi_center") || center == "" {
		return nil
	}
	return denmaCenterSends.Get(center)
}

// denmaCenterRoom is n, or the room left under the center's cap if less.
func denmaCenterRoom(ko *koanf.Koanf, n int) int {
	if cc := denmaCenterCounter(ko); cc != nil {
		if left := cc.Left(); left >= 0 {
			return min(n, left)
		}
	}
	return n
}

// denmaCenterCap sets a center's cap from its settings (ko), and makes its
// campaign messages wait for it. Called by buildApp, before the manager
// runs. (Its Config page sets a new cap at once, as a save doesn't reload a
// center that's sending, cmd/denma_center.go.)
func denmaCenterCap(mgr *manager.Manager, ko *koanf.Koanf) {
	if c := denmaCenterCounter(ko); c != nil {
		c.SetLimit(ko.Int("denma.daily_cap"), 0)
		mgr.DenmaSetCenterWait(ko.String("denma.center"), func(stopped func() bool) (bool, func()) { return c.Wait("", stopped) })
	}
	denmaCenterWindow(mgr, ko) // and its sending hours (cmd/denma_sendwindow.go)
}

// denmaCenterClaimHeader marks an e-mail as claimed against the center's cap
// (Counter.Claim, by automations and the API), with the claim's ID; it's
// taken off before the e-mail is sent.
const denmaCenterClaimHeader = "X-Denma-Center-Claim"

// denmaCountedMessenger counts what a center's manager sends against its cap,
// once an API campaign's e-mail has passed its link check, and a campaign's
// or automation's e-mail shows the center's postal address.
type denmaCountedMessenger struct {
	manager.Messenger
	count *denmadaily.Counter
	slug  string
}

func (m denmaCountedMessenger) Push(msg models.Message) error {
	if err := denmaCheckCampaignLinks(m.slug, msg); err != nil { // cmd/denma_api_send.go
		return err
	}
	if m.Name() == emailMsgr && denmaIsMarketing(msg) { // the center's details (cmd/denma_footer.go)
		if err := denmaFooterCheckSent(m.slug, msg.Body); err != nil {
			return err
		}
	}
	if msg.Headers != nil && msg.Headers.Get(manager.DenmaCenterCounted) != "" {
		msg.Headers.Del(manager.DenmaCenterCounted) // counted by its wait
	} else if claim, ok := denmaClaimOf(msg, denmaCenterClaimHeader); ok {
		m.count.AddClaimed(claim)
	} else {
		m.count.Add()
	}
	return m.Messenger.Push(msg)
}

// denmaCountSending counts a center's messengers' sends against its cap.
func denmaCountSending(msgrs []manager.Messenger, ko *koanf.Koanf) []manager.Messenger {
	c := denmaCenterCounter(ko)
	if c == nil {
		return msgrs
	}
	out := make([]manager.Messenger, len(msgrs))
	for i, m := range msgrs {
		out[i] = denmaCountedMessenger{Messenger: m, count: c, slug: ko.String("denma.center")}
	}
	return out
}

// denmaCapStatus is a center's sending against its cap, for its Config page
// and the API.
type denmaCapStatus struct {
	Cap  int `json:"daily_cap"` // 0: none
	Sent int `json:"sent_24h"`
	Left int `json:"left"` // -1 without a cap
}

func (a *App) denmaCapStatus() denmaCapStatus {
	c := denmaCenterCounter(a.ko)
	if c == nil {
		return denmaCapStatus{Left: -1}
	}
	s := c.Status()
	out := denmaCapStatus{Cap: s.Limit, Sent: s.Sent, Left: -1}
	if s.Limit > 0 {
		out.Left = s.Left
	}
	return out
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
	// And the activity log's (cmd/denma_audit.go).
	if set.DenmaAuditDays < 0 || set.DenmaAuditDays > denmaAuditMaxDays {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("The activity log keeps its entries from 1 to %d days, or for ever (0).", denmaAuditMaxDays))
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
