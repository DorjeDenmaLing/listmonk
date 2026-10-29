package main

// denma: how the sending API's transactional e-mails go after they're
// queued (POST /v1/messages, cmd/denma_api_send.go). Each is kept in
// denma_api_outbox from when it's queued until the mail server answers:
//
//   - sent: forgotten.
//   - refused for good: a message_failed event for the key.
//   - "try later" or the server unreachable: tried again after each of
//     denmaRetryWaits (as automations' e-mails are), then a message_failed
//     event.
//   - the connection lost before the server's answer: a message_uncertain
//     event, never sent again (it may have been delivered).
//
// A retry claims the hub's daily limit and the center's cap as a new e-mail
// does. Events say only what happened, never the mail server's words. The
// minute's job (cmd/denma_api_events.go) sends the retries, and sees to
// e-mails left by a restart: those not taken by a worker go again, those
// being sent are uncertain.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

// denmaAPIOutboxSQL makes the table (denmaAPISQL), since features version 56.
const denmaAPIOutboxSQL = `
CREATE TABLE IF NOT EXISTS denma_api_outbox (
    id            BIGSERIAL PRIMARY KEY,
    key_id        INTEGER NOT NULL REFERENCES denma_api_keys(id) ON DELETE CASCADE,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    email         TEXT NOT NULL,
    template_id   INTEGER NULL,
    from_email    TEXT NOT NULL,
    reply_to      TEXT NOT NULL DEFAULT '',
    subject       TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    body          TEXT NOT NULL,
    alt_body      TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'queued', -- queued, sending, retry
    retries       INTEGER NOT NULL DEFAULT 0,
    next_at       TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_api_outbox_retry ON denma_api_outbox (next_at) WHERE status = 'retry';
CREATE INDEX IF NOT EXISTS denma_api_outbox_open ON denma_api_outbox (updated_at) WHERE status <> 'retry';
`

const (
	denmaAPIEventFailed    = "message_failed"
	denmaAPIEventUncertain = "message_uncertain"

	denmaAPIRetryBatch = 100               // e-mails tried again a minute
	denmaAPIRetryHold  = 5                 // minutes an e-mail no worker took waits
	denmaAPIOutboxDays = 1                 // days a retry may wait (the API paused, a sender not ready)
	denmaAPIMsgDropped = "It wasn't sent." // the address can't be e-mailed now
	denmaAPIMsgRefused = "The mail server refused it."
	denmaAPIMsgMaybe   = "It may have been delivered: the connection was lost before the mail server answered. It isn't sent again."
)

// errDenmaAPIRestart is the outcome of an e-mail that was being sent when the
// process stopped.
var errDenmaAPIRestart = errors.New("the server stopped while it was being sent")

// denmaAPIMsgBusy is the reason once the mail server couldn't take an e-mail
// after all its tries.
func denmaAPIMsgBusy() string {
	return fmt.Sprintf("The mail server couldn't take it, after %d tries.", len(denmaRetryWaits)+1)
}

// What becomes of a sent e-mail.
const (
	denmaAPISent      = "sent"
	denmaAPIRetry     = "retry"
	denmaAPIFailed    = "failed"
	denmaAPIUncertain = "uncertain"
)

// denmaAPIVerdict says what becomes of an e-mail the mail server answered
// err to, after retries tries again, and the reason to give for an event.
func denmaAPIVerdict(err error, retries int) (verdict, reason string) {
	switch {
	case err == nil:
		return denmaAPISent, ""
	case manager.DenmaUncertain(err) || errors.Is(err, errDenmaAPIRestart):
		return denmaAPIUncertain, denmaAPIMsgMaybe
	case !manager.DenmaRetryable(err):
		return denmaAPIFailed, denmaAPIMsgRefused
	case retries < len(denmaRetryWaits):
		return denmaAPIRetry, ""
	}
	return denmaAPIFailed, denmaAPIMsgBusy()
}

// denmaAPIMessage is an API e-mail's message to a subscriber.
func denmaAPIMessage(sub models.Subscriber, from, replyTo, subject, contentType string, body []byte, alt string, attach []models.Attachment) models.Message {
	h := textproto.MIMEHeader{}
	h.Set(models.EmailHeaderSubscriberUUID, sub.UUID) // so its bounces and complaints find it (cmd/denma_bounces.go)
	if replyTo != "" {
		h.Set("Reply-To", replyTo)
	}
	return models.Message{
		Subscriber:  sub,
		To:          []string{sub.Email},
		From:        from,
		Subject:     subject,
		ContentType: contentType,
		Messenger:   emailMsgr,
		Body:        body,
		AltBody:     []byte(alt),
		Attachments: attach,
		Headers:     h,
	}
}

// apiOutboxAdd records a queued e-mail (key keyID, template tplID), and
// returns its ID.
func (a *App) apiOutboxAdd(keyID, tplID int, msg models.Message) (int64, error) {
	var id int64
	err := a.db.Get(&id, `INSERT INTO denma_api_outbox (key_id, subscriber_id, email, template_id, from_email, reply_to, subject, content_type, body, alt_body)
		VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6, $7, $8, $9, $10) RETURNING id`,
		keyID, msg.Subscriber.ID, strings.ToLower(msg.To[0]), tplID, msg.From, msg.Headers.Get("Reply-To"), msg.Subject, msg.ContentType,
		string(msg.Body), string(msg.AltBody))
	return id, err
}

// apiOutboxPush hands a recorded e-mail to the campaign manager, which tells
// the outbox how it went.
func (a *App) apiOutboxPush(id int64, msg models.Message) error {
	return a.manager.DenmaPushTracked(msg, func() error {
		res, err := a.db.Exec(`UPDATE denma_api_outbox SET status = 'sending', updated_at = NOW() WHERE id = $1 AND status = 'queued'`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("the e-mail changed")
		}
		return nil
	}, func(err error) { a.apiOutboxOutcome(id, err) })
}

// apiOutboxForget deletes a recorded e-mail that wasn't queued after all.
func (a *App) apiOutboxForget(id int64) {
	if _, err := a.db.Exec(`DELETE FROM denma_api_outbox WHERE id = $1`, id); err != nil {
		a.log.Printf("denma: error forgetting an API e-mail: %v", err)
	}
}

// apiOutboxEnd deletes e-mail id, if it's being sent, with an event for its
// key: type and reason.
func (a *App) apiOutboxEnd(id int64, typ, reason string) {
	if _, err := a.db.Exec(`WITH d AS (DELETE FROM denma_api_outbox WHERE id = $1 AND status = 'sending' RETURNING key_id, email)
		INSERT INTO denma_api_events (type, key_id, email, reason) SELECT $2, key_id, email, $3 FROM d`, id, typ, reason); err != nil {
		a.log.Printf("denma: error recording an API e-mail's outcome: %v", err)
	}
}

// apiOutboxOutcome records the mail server's answer to e-mail id.
func (a *App) apiOutboxOutcome(id int64, err error) {
	var retries int
	if e := a.db.Get(&retries, `SELECT retries FROM denma_api_outbox WHERE id = $1 AND status = 'sending'`, id); e != nil {
		if !errors.Is(e, sql.ErrNoRows) {
			a.log.Printf("denma: error reading an API e-mail's outcome: %v", e)
		}
		return
	}
	verdict, reason := denmaAPIVerdict(err, retries)
	switch verdict {
	case denmaAPISent:
		a.apiOutboxForget(id)
	case denmaAPIRetry:
		if _, e := a.db.Exec(`UPDATE denma_api_outbox SET status = 'retry', retries = retries + 1, updated_at = NOW(),
			next_at = NOW() + make_interval(mins => ($2::INT[])[retries + 1]) WHERE id = $1 AND status = 'sending'`,
			id, pq.Array(denmaRetryWaits)); e != nil {
			a.log.Printf("denma: error recording an API e-mail to try again: %v", e)
		}
	case denmaAPIUncertain:
		a.apiOutboxEnd(id, denmaAPIEventUncertain, reason)
	default:
		a.apiOutboxEnd(id, denmaAPIEventFailed, reason)
	}
}

// denmaAPIOutboxRow is a recorded e-mail to try again.
type denmaAPIOutboxRow struct {
	ID          int64  `db:"id"`
	KeyID       int    `db:"key_id"`
	SubID       int    `db:"subscriber_id"`
	Email       string `db:"email"`
	TemplateID  int    `db:"template_id"`
	From        string `db:"from_email"`
	ReplyTo     string `db:"reply_to"`
	Subject     string `db:"subject"`
	ContentType string `db:"content_type"`
	Body        string `db:"body"`
	AltBody     string `db:"alt_body"`
}

// apiOutboxLater puts a retry that wasn't sent back, to be tried again in
// denmaAPIRetryHold minutes (not counted as a try).
func (a *App) apiOutboxLater(id int64) {
	if _, err := a.db.Exec(`UPDATE denma_api_outbox SET status = 'retry', next_at = NOW() + make_interval(mins => $2), updated_at = NOW()
		WHERE id = $1 AND status = 'queued'`, id, denmaAPIRetryHold); err != nil {
		a.log.Printf("denma: error putting an API e-mail back: %v", err)
	}
}

// apiOutboxDrop ends a queued retry that won't be sent, with an event.
func (a *App) apiOutboxDrop(id int64, reason string) {
	if _, err := a.db.Exec(`WITH d AS (DELETE FROM denma_api_outbox WHERE id = $1 AND status = 'queued' RETURNING key_id, email)
		INSERT INTO denma_api_events (type, key_id, email, reason) SELECT $2, key_id, email, $3 FROM d`, id, denmaAPIEventFailed, reason); err != nil {
		a.log.Printf("denma: error recording an API e-mail's outcome: %v", err)
	}
}

// denmaAPIRecovered holds the centers whose e-mails left by the last
// process have been seen to.
var denmaAPIRecovered sync.Map

// apiRecoverMessages sees to e-mails left queued or being sent: by the last
// process (once in this one), or for longer than denmaAutoStale. Queued ones
// weren't sent, and go again; those being sent may have been delivered.
func (a *App) apiRecoverMessages() {
	before := time.Now().Add(-denmaAutoStale)
	if _, done := denmaAPIRecovered.LoadOrStore(a.ko.String("denma.center"), true); !done {
		before = denmaProcessStart
	}
	if _, err := a.db.Exec(`UPDATE denma_api_outbox SET status = 'retry', next_at = NOW(), updated_at = NOW()
		WHERE status = 'queued' AND updated_at < $1`, before); err != nil {
		a.log.Printf("denma: error seeing to the API's unsent e-mails: %v", err)
	}
	res, err := a.db.Exec(`WITH d AS (DELETE FROM denma_api_outbox WHERE status = 'sending' AND updated_at < $1 RETURNING key_id, email)
		INSERT INTO denma_api_events (type, key_id, email, reason) SELECT $2, key_id, email, $3 FROM d`, before, denmaAPIEventUncertain, denmaAPIMsgMaybe)
	if err != nil {
		a.log.Printf("denma: error seeing to the API's e-mails being sent: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		a.log.Printf("denma: %d API e-mails were being sent when the server stopped (or for over an hour): marked as maybe delivered", n)
	}
}

// apiRetryMessages sends the e-mails whose wait is over, up to
// denmaAPIRetryBatch, if the API isn't paused and the daily limit and the
// center's cap have room. The minute's job calls it.
func (a *App) apiRetryMessages() {
	// A leaked key's aren't sent; the ones waiting too long fail.
	if _, err := a.db.Exec(`DELETE FROM denma_api_outbox o USING denma_api_keys k
		WHERE k.id = o.key_id AND k.deleted_at IS NOT NULL AND k.leaked`); err != nil {
		a.log.Printf("denma: error forgetting a leaked key's e-mails: %v", err)
	}
	if _, err := a.db.Exec(`WITH d AS (DELETE FROM denma_api_outbox WHERE status = 'retry' AND created_at < NOW() - make_interval(days => $1) RETURNING key_id, email)
		INSERT INTO denma_api_events (type, key_id, email, reason) SELECT $2, key_id, email, $3 FROM d`,
		denmaAPIOutboxDays, denmaAPIEventFailed, denmaAPIMsgBusy()); err != nil {
		a.log.Printf("denma: error ending the API's old e-mails: %v", err)
	}
	if st, err := a.apiState(); err != nil || st.PausedAt != nil {
		return
	}
	// Those due, whose sender's domain is ready.
	var froms []string
	if err := a.db.Select(&froms, `SELECT DISTINCT from_email FROM denma_api_outbox WHERE status = 'retry' AND next_at <= NOW()`); err != nil {
		a.log.Printf("denma: error finding the API's e-mails to try again: %v", err)
		return
	}
	if len(froms) == 0 {
		a.apiOutboxWant(0, 0, nil)
		return
	}
	waiting := []string{}
	for _, f := range froms {
		if err := a.denmaCheckSenderReady(f); err != nil { // cmd/denma_domains.go
			waiting = append(waiting, f)
		}
	}

	// Claimed as new e-mails are, before any is sent.
	limit := denmaAPIRetryBatch
	got, claim := denmaDaily.Claim(limit)
	a.apiOutboxWant(limit, got, waiting)
	if got == 0 {
		return
	}
	limit = got
	pushed := 0
	defer func() { denmaDaily.Release(claim, got-pushed) }()
	var centerClaim int64
	if cc := denmaCenterCounter(a.ko); cc != nil {
		cgot, id := cc.Claim(limit)
		if cgot == 0 {
			return
		}
		limit, centerClaim = cgot, id
		defer func() { cc.Release(centerClaim, cgot-pushed) }()
	}

	var rows []denmaAPIOutboxRow
	if err := a.db.Select(&rows, `UPDATE denma_api_outbox o SET status = 'queued', updated_at = NOW() FROM (
			SELECT x.id FROM denma_api_outbox x WHERE x.status = 'retry' AND x.next_at <= NOW() AND x.from_email <> ALL($2)
			ORDER BY x.next_at LIMIT $1 FOR UPDATE SKIP LOCKED) d
		WHERE o.id = d.id
		RETURNING o.id, o.key_id, o.subscriber_id, o.email, COALESCE(o.template_id, 0) AS template_id, o.from_email, o.reply_to,
			o.subject, o.content_type, o.body, o.alt_body`, limit, pq.Array(waiting)); err != nil {
		a.log.Printf("denma: error taking the API's e-mails to try again: %v", err)
		return
	}
	skip := denmaEmailSkip(a.ko)
	for _, r := range rows {
		var sub models.Subscriber
		if err := a.db.Get(&sub, `SELECT id, uuid, email, name, first_name, last_name, attribs, status, created_at, updated_at FROM subscribers WHERE id = $1`, r.SubID); err != nil {
			a.apiOutboxLater(r.ID)
			continue
		}
		// Still someone it may be sent to.
		if no, err := a.denmaTxRefused(strings.ToLower(sub.Email), skip); err != nil {
			a.apiOutboxLater(r.ID)
			continue
		} else if no {
			a.apiOutboxDrop(r.ID, denmaAPIMsgDropped)
			continue
		}
		var attach []models.Attachment
		if r.TemplateID > 0 {
			if tpl, err := a.manager.GetTpl(r.TemplateID); err == nil {
				attach = tpl.Attachments
			}
		}
		msg := denmaAPIMessage(sub, r.From, r.ReplyTo, r.Subject, r.ContentType, []byte(r.Body), r.AltBody, attach)
		denmaMarkClaim(&msg, denmaClaimHeader, claim)
		denmaMarkClaim(&msg, denmaCenterClaimHeader, centerClaim)
		if err := a.apiOutboxPush(r.ID, msg); err != nil {
			a.apiOutboxLater(r.ID)
			continue
		}
		pushed++
	}
}

// apiOutboxWant asks the daily limit to keep room, ahead of campaigns, for
// the retries due that its claim didn't cover and that may go (not from a
// sender in waiting, within the center's cap); none clears it.
func (a *App) apiOutboxWant(limit, got int, waiting []string) {
	due := 0
	if got < limit {
		if err := a.db.Get(&due, `SELECT COUNT(*) FROM (SELECT 1 FROM denma_api_outbox
			WHERE status = 'retry' AND next_at <= NOW() AND from_email <> ALL($2) LIMIT $1) x`, limit, pq.Array(waiting)); err != nil {
			a.log.Printf("denma: error counting the API's e-mails to try again: %v", err)
			return
		}
	}
	denmaDaily.Want(a.ko.String("denma.center")+"/api-outbox", max(denmaCenterRoom(a.ko, due)-got, 0))
}
