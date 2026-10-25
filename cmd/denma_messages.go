package main

// denma: every e-mail a center sends to one of its subscribers, with the mail
// server's answer (denma_messages), shown on the subscriber's Activity tab:
// campaigns (and their tests), automations, and others (opt-in
// confirmations, transactional e-mails, notices), each with its subject and
// whether it was sent, failed or may have been delivered. Kept for
// denmaMessageDays (pruned hourly with expired sessions, cmd/denma_prune.go),
// and with the subscriber: deleting them deletes their record. Automations
// read it for "no other e-mail in the last N days" (cmd/denma_automations.go).
//
// Each center's e-mail messenger records what it sends (email.DenmaOnSent),
// so nothing that sends needs to: an e-mail to an address that isn't a
// subscriber of the center (a staff notice) isn't recorded. Campaign
// messages carry their campaign; automations' e-mails say which, and which
// of its e-mails, in an internal header (denmaSentAsHeader), which isn't
// sent.

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/models"
	null "gopkg.in/volatiletech/null.v6"
)

// denmaMessageDays is how long a subscriber's e-mails are recorded for.
const denmaMessageDays = 90

// denmaSentAsHeader says which automation, and which of its e-mails, an
// e-mail is: "automation <id> <step>" (id 0 for a test).
const denmaSentAsHeader = "X-Denma-Sent-As"

// What an e-mail was.
const (
	denmaMsgCampaign   = "campaign"
	denmaMsgAutomation = "automation"
	denmaMsgTest       = "test"
	denmaMsgOther      = "other"
)

// denmaMessagesSQL makes the record (in denmaFeaturesSQL).
const denmaMessagesSQL = `
CREATE TABLE IF NOT EXISTS denma_messages (
    id            BIGSERIAL PRIMARY KEY,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL, -- campaign, automation, test, other
    campaign_id   INTEGER NULL REFERENCES campaigns(id) ON DELETE SET NULL,
    automation_id INTEGER NULL,
    step          INTEGER NULL, -- which of the automation's e-mails
    subject       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL, -- sent, failed, uncertain (may have been delivered)
    error         TEXT NOT NULL DEFAULT '',
    sent_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_messages_sub ON denma_messages (subscriber_id, sent_at);
CREATE INDEX IF NOT EXISTS denma_messages_at ON denma_messages (sent_at);
`

// denmaSentAs marks an automation's e-mail (id 0 for a test) for the record.
func denmaSentAs(m *models.Message, autoID, step int) {
	m.Headers.Set(denmaSentAsHeader, fmt.Sprintf("automation %d %d", autoID, step))
}

// denmaMessageKind is what an e-mail was, with its campaign or automation and
// step.
func denmaMessageKind(m models.Message) (kind string, campID, autoID, step null.Int) {
	if m.Campaign != nil && m.Campaign.ID > 0 {
		kind = denmaMsgCampaign
		if m.Headers.Get(manager.DenmaCampaignTest) != "" {
			kind = denmaMsgTest
		}
		return kind, null.IntFrom(m.Campaign.ID), autoID, step
	}
	if f := strings.Fields(m.Headers.Get(denmaSentAsHeader)); len(f) == 3 && f[0] == denmaMsgAutomation {
		id, _ := strconv.Atoi(f[1])
		n, _ := strconv.Atoi(f[2])
		if id == 0 {
			return denmaMsgTest, campID, autoID, step
		}
		return denmaMsgAutomation, campID, null.IntFrom(id), null.IntFrom(n)
	}
	return denmaMsgOther, campID, autoID, step
}

// denmaIsMarketing reports whether an e-mail is a campaign's or an
// automation's (tests included).
func denmaIsMarketing(m models.Message) bool {
	return (m.Campaign != nil && m.Campaign.ID > 0) || m.Headers.Get(denmaSentAsHeader) != ""
}

// denmaLogMessages has a center's e-mail messenger (em) record each e-mail it
// sends to one of the center's subscribers. Called by buildApp.
// denmaLogMessageTimeout bounds recording one, so that a slow center
// database can't hold up the send workers every center shares.
const denmaLogMessageTimeout = 2 * time.Second

func denmaLogMessages(em *email.Emailer, db *sqlx.DB, ko *koanf.Koanf) {
	if em == nil || !ko.Bool("denma.multi_center") || ko.String("denma.center") == "" {
		return
	}
	l := denmaLog(ko)
	em.DenmaOnSent(func(m models.Message, err error) {
		kind, campID, autoID, step := denmaMessageKind(m)
		status, errText := "sent", ""
		if err != nil {
			status, errText = "failed", err.Error()
			if manager.DenmaUncertain(err) {
				status = "uncertain"
			}
		}
		to := ""
		if len(m.To) > 0 {
			to = m.To[0]
			if a, err := mail.ParseAddress(to); err == nil {
				to = a.Address
			}
		}
		// Bounded: it runs in the shared send path.
		ctx, cancel := context.WithTimeout(context.Background(), denmaLogMessageTimeout)
		defer cancel()
		if _, err := db.ExecContext(ctx, `INSERT INTO denma_messages (subscriber_id, kind, campaign_id, automation_id, step, subject, status, error)
			SELECT s.id, $3, $4, $5, $6, $7, $8, $9 FROM subscribers s
			WHERE s.id = (CASE WHEN $1::INT > 0 THEN $1::INT ELSE (SELECT id FROM subscribers WHERE LOWER(email) = LOWER($2) LIMIT 1) END)`,
			m.Subscriber.ID, to, kind, campID, autoID, step, m.Subject, status, errText); err != nil {
			l.Printf("denma: error recording a sent e-mail: %v", err)
		}
	})
}

// denmaMessage is an e-mail sent to a subscriber, for their Activity tab.
type denmaMessage struct {
	Kind         string    `db:"kind"`
	CampaignID   null.Int  `db:"campaign_id"`
	CampaignName string    `db:"campaign_name"`
	AutoID       null.Int  `db:"automation_id"`
	AutoName     string    `db:"automation_name"`
	Step         null.Int  `db:"step"`
	Subject      string    `db:"subject"`
	Status       string    `db:"status"`
	Error        string    `db:"error"`
	SentAt       time.Time `db:"sent_at"`
}

// denmaSubscriberMessages is the latest e-mails sent to a subscriber.
func (a *App) denmaSubscriberMessages(subID int) []denmaMessage {
	if a.ko.String("denma.center") == "" {
		return nil
	}
	var out []denmaMessage
	if err := a.db.Select(&out, `SELECT m.kind, m.campaign_id, COALESCE(c.name, '') AS campaign_name,
			m.automation_id, COALESCE(au.name, '') AS automation_name, m.step, m.subject, m.status, m.error, m.sent_at
		FROM denma_messages m
		LEFT JOIN campaigns c ON c.id = m.campaign_id
		LEFT JOIN denma_automations au ON au.id = m.automation_id
		WHERE m.subscriber_id = $1 ORDER BY m.sent_at DESC, m.id DESC LIMIT 100`, subID); err != nil {
		a.log.Printf("denma: error getting subscriber %d's e-mails: %v", subID, err)
	}
	return out
}

// denmaMessagesWithin is the SQL condition that subscriber s had a campaign's
// or another automation's e-mail (than automation $auto) in the last $days
// days.
func denmaMessagesWithin(sub, days, auto string) string {
	return `EXISTS (SELECT 1 FROM denma_messages dm WHERE dm.subscriber_id = ` + sub + `
        AND dm.kind IN ('campaign', 'automation') AND dm.status IN ('sent', 'uncertain')
        AND dm.automation_id IS DISTINCT FROM ` + auto + ` AND dm.sent_at > NOW() - make_interval(days => ` + days + `))`
}
