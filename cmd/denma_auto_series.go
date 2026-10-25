package main

// denma: an automation's follow-up e-mails (a series), and how its e-mails
// are sent (cmd/denma_automations.go).
//
// Series. Besides its e-mail, an automation may send up to
// denmaAutoMaxSteps more, each a wait after the one before
// (denma_automation_steps, numbered from 2). Its lists and tags change only
// once, with the first. Before each follow-up the person must still be
// someone it may e-mail (not blocklisted, which unsubscribing does; not
// fenced without consent; subscribed to something) and meet its conditions
// (if_tags, unless_tags), so "not if they have the tag" ends a series;
// otherwise it stops there. Turned off, its series wait; turned on again,
// those due go. An "each time" automation set off again starts its series
// again.
//
// A run's row (denma_automation_sends: one per automation and person) is at
// a step, with a status:
//   - done, skipped, failed, sent: finished (sent: its last e-mail went).
//   - waiting: for e-mail `step`, at next_at.
//   - queued: its e-mail is being handed to the campaign manager.
//   - sending: a worker has taken it; the mail server hasn't answered yet.
//   - retry: e-mail `step` wasn't sent (the server said "try later", or
//     couldn't be reached), and is tried again at next_at, up to
//     len(denmaRetryWaits) times, as campaigns' failed sends are.
//
// Delivery. An e-mail is handed over only once a worker takes it
// (manager.DenmaPushTaken), which records it as sending first; until then
// it can be withdrawn, unsent, and is tried again. The mail server's answer
// is recorded: sent; or failed (refused for good); or, if the connection was
// lost before its answer, maybe delivered (uncertain), which isn't sent
// again, as that could be a second copy. A restart finds the runs it left:
// queued ones weren't sent and go again; sending ones may have been
// delivered. A shutdown waits for those being sent (cmd/denma_shutdown.go).
//
// History. denma_automation_log keeps every step of every run, with what it
// did: the subscriber's Activity tab, the schedule and the counts read it.
// It's kept as long as the person and the automation are.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	// denmaAutoMaxSteps is the most follow-up e-mails.
	denmaAutoMaxSteps = 10
	// denmaAutoTakeWait is how long an e-mail waits for a worker to take it.
	denmaAutoTakeWait = 30 * time.Second
	// denmaAutoUntakenMins is the wait, in minutes, before an e-mail no worker
	// took is tried again (not counted as a retry).
	denmaAutoUntakenMins = 5
)

// denmaAutoSeriesSQL makes the series' and history's tables (in
// denmaAutoTables).
const denmaAutoSeriesSQL = `
CREATE TABLE IF NOT EXISTS denma_automation_steps (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    step          INTEGER NOT NULL, -- 2, 3, ...
    delay_minutes INTEGER NOT NULL, -- after the e-mail before it
    template_id   INTEGER NULL REFERENCES templates(id) ON DELETE SET NULL,
    subject       TEXT NOT NULL,
    PRIMARY KEY (automation_id, step)
);
ALTER TABLE denma_automation_sends
    ADD COLUMN IF NOT EXISTS step    INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS next_at TIMESTAMP WITH TIME ZONE NULL,
    ADD COLUMN IF NOT EXISTS retries INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS denma_automation_sends_next ON denma_automation_sends (automation_id, next_at) WHERE next_at IS NOT NULL;
DO $$ BEGIN
    IF to_regclass('denma_automation_log') IS NULL THEN
        CREATE TABLE denma_automation_log (
            id            BIGSERIAL PRIMARY KEY,
            automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
            subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
            run           INTEGER NOT NULL, -- the run's number (denma_automation_sends.runs)
            step          INTEGER NOT NULL,
            status        TEXT NOT NULL, -- sent, uncertain, done, failed, skipped
            did           TEXT NOT NULL DEFAULT '',
            error         TEXT NOT NULL DEFAULT '',
            at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
        );
        CREATE INDEX denma_automation_log_auto ON denma_automation_log (automation_id, at);
        CREATE INDEX denma_automation_log_sub ON denma_automation_log (subscriber_id);
        -- What was recorded before: each person's last run.
        INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did, error, at)
            SELECT automation_id, subscriber_id, runs, 1, status, did, error, sent_at FROM denma_automation_sends;
    END IF;
END $$;`

// denmaAutoStep is one of an automation's follow-up e-mails.
type denmaAutoStep struct {
	Step         int      `json:"step"`
	DelayMinutes int      `json:"delay_minutes"`
	TemplateID   null.Int `json:"template_id"`
	Subject      string   `json:"subject"`
	TemplateName string   `json:"template_name"`
}

// denmaAutoSteps is an automation's follow-ups, read as JSON
// (denmaAutoStepsSQL).
type denmaAutoSteps []denmaAutoStep

func (s *denmaAutoSteps) Scan(v any) error {
	switch b := v.(type) {
	case []byte:
		return json.Unmarshal(b, s)
	case string:
		return json.Unmarshal([]byte(b), s)
	}
	return fmt.Errorf("can't read an automation's steps from %T", v)
}

// denmaAutoStepsSQL is automation a's follow-ups, as JSON.
const denmaAutoStepsSQL = `COALESCE((SELECT json_agg(json_build_object('step', s.step, 'delay_minutes', s.delay_minutes,
        'template_id', s.template_id, 'subject', s.subject, 'template_name', COALESCE(st.name, '')) ORDER BY s.step)
        FROM denma_automation_steps s LEFT JOIN templates st ON st.id = s.template_id WHERE s.automation_id = a.id), '[]')`

// denmaAutoStepForm is a follow-up as the automation page saves it.
type denmaAutoStepForm struct {
	DelayMinutes int    `json:"delay_minutes"`
	TemplateID   int    `json:"template_id"`
	Subject      string `json:"subject"`
}

// stepEmail is e-mail n's template and subject (1: the automation's own),
// and whether there's one.
func (d denmaAutomation) stepEmail(n int) (int, string, bool) {
	if n == 1 {
		return int(d.TemplateID.Int), d.Subject, d.TemplateID.Valid
	}
	for _, s := range d.Steps {
		if s.Step == n {
			return int(s.TemplateID.Int), s.Subject, s.TemplateID.Valid
		}
	}
	return 0, "", false
}

// validateAutoSteps checks and tidies a saved or tested automation's
// follow-ups.
func (a *App) validateAutoSteps(f *denmaAutoForm) error {
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	if !f.SendEmail {
		f.Steps = nil
		return nil
	}
	if len(f.Steps) > denmaAutoMaxSteps {
		return bad(fmt.Sprintf("An automation can send at most %d follow-up e-mails.", denmaAutoMaxSteps))
	}
	for i := range f.Steps {
		s := &f.Steps[i]
		n := i + 2
		s.Subject = strings.TrimSpace(s.Subject)
		if s.DelayMinutes < 1 || s.DelayMinutes > denmaAutoMaxDelay {
			return bad(fmt.Sprintf("E-mail %d's wait can be from a minute to a year.", n))
		}
		if !strHasLen(s.Subject, 1, 500) {
			return bad(fmt.Sprintf("Enter e-mail %d's subject.", n))
		}
		tpl, err := a.autoTemplate(s.TemplateID, false)
		if err != nil {
			return bad(fmt.Sprintf("Choose e-mail %d's template: a transactional or visual one.", n))
		}
		msg, err := a.autoMessage(denmaAutomation{UUID: dummyUUID, Name: f.Name, Subject: s.Subject}, n, tpl, denmaAutoSample)
		if err != nil {
			return bad(fmt.Sprintf("E-mail %d: %v", n, err))
		}
		if err := a.denmaCheckMessage(msg.Body); err != nil { // cmd/denma_address.go
			return bad(fmt.Sprintf("E-mail %d: %v", n, err))
		}
	}
	return nil
}

// saveAutoSteps replaces an automation's follow-ups.
func saveAutoSteps(tx *sqlx.Tx, id int, steps []denmaAutoStepForm) error {
	if _, err := tx.Exec(`DELETE FROM denma_automation_steps WHERE automation_id = $1`, id); err != nil {
		return err
	}
	for i, s := range steps {
		if _, err := tx.Exec(`INSERT INTO denma_automation_steps (automation_id, step, delay_minutes, template_id, subject)
			VALUES ($1, $2, $3, $4, $5)`, id, i+2, s.DelayMinutes, s.TemplateID, s.Subject); err != nil {
			return err
		}
	}
	return nil
}

// autoLog records a step of a run in the history.
func (a *App) autoLog(autoID, subID, run, step int, status, did, errText string) {
	if _, err := a.db.Exec(`INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, autoID, subID, run, step, status, did, errText); err != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", autoID, err)
	}
}

// autoStop ends a queued run (status: failed or skipped), recording why.
func (a *App) autoStop(autoID, subID, run, step int, status, did, errText string) {
	if _, err := a.db.Exec(`WITH d AS (UPDATE denma_automation_sends SET status = $5, did = $6, error = $7, next_at = NULL, sent_at = NOW()
			WHERE automation_id = $1 AND subscriber_id = $2 AND runs = $3 AND step = $4 AND status = 'queued' RETURNING 1)
		INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did, error)
		SELECT $1, $2, $3, $4, $5, $6, $7 FROM d`, autoID, subID, run, step, status, did, errText); err != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", autoID, err)
	}
}

// autoSend sends e-mail `step` of auto (with its subject) to subs, whose runs
// are queued, each once a worker has taken it, and returns how many were.
// Each one's outcome is recorded when the mail server answers (autoOutcome);
// one no worker took is tried again in denmaAutoUntakenMins.
func (a *App) autoSend(auto denmaAutomation, step int, tpl denmaAutoTemplate, subs []models.Subscriber, runs map[int]int,
	did func(int) string, claim, centerClaim int64) int {
	pushed := 0
	for i, s := range subs {
		run, sdid := runs[s.ID], did(s.ID)
		msg, err := a.autoMessage(auto, step, tpl, s)
		if err != nil {
			a.autoStop(auto.ID, s.ID, run, step, "failed", sdid, err.Error())
			continue
		}
		denmaMarkClaim(&msg, denmaClaimHeader, claim)             // counted against its claim when it goes (cmd/denma_sending.go)
		denmaMarkClaim(&msg, denmaCenterClaimHeader, centerClaim) // cmd/denma_daily.go
		taken := a.manager.DenmaPushTaken(msg, denmaAutoTakeWait, func() error {
			res, err := a.db.Exec(`UPDATE denma_automation_sends SET status = 'sending', sent_at = NOW()
				WHERE automation_id = $1 AND subscriber_id = $2 AND runs = $3 AND step = $4 AND status = 'queued'`, auto.ID, s.ID, run, step)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("the run changed")
			}
			return nil
		}, func(err error) { a.autoOutcome(auto.ID, s.ID, run, step, sdid, err) })
		if !taken {
			// The workers are busy (or stopping): this one and the rest go
			// later, and its claim on the daily limit is given back.
			rest := make([]int, 0, len(subs)-i)
			for _, r := range subs[i:] {
				rest = append(rest, r.ID)
			}
			a.autoRetryLater(auto.ID, rest, step)
			break
		}
		pushed++
	}
	return pushed
}

// autoSkipEmail records that e-mail `step` of runs (being started, or
// queued) wasn't sent, with what it did (did), and moves them on to the next
// e-mail, if there's one.
func (a *App) autoSkipEmail(autoID, step int, subIDs []int, did string) {
	if _, err := a.db.Exec(`WITH u AS (UPDATE denma_automation_sends d SET
			step = COALESCE(s.step, d.step),
			status = CASE WHEN s.step IS NULL THEN 'skipped' ELSE 'waiting' END,
			next_at = CASE WHEN s.step IS NOT NULL THEN NOW() + make_interval(mins => s.delay_minutes) END,
			retries = 0, error = '', did = $4, sent_at = NOW()
		FROM (SELECT 1) x LEFT JOIN denma_automation_steps s ON s.automation_id = $1 AND s.step = $2 + 1
		WHERE d.automation_id = $1 AND d.subscriber_id = ANY($3) AND d.step = $2 AND d.status IN ('done', 'queued')
		RETURNING d.subscriber_id, d.runs)
		INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did)
		SELECT $1, subscriber_id, runs, $2, 'skipped', $4 FROM u`, autoID, step, pq.Array(subIDs), did); err != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", autoID, err)
	}
}

// autoRetryLater puts queued runs at e-mail `step` back, to be tried again
// in denmaAutoUntakenMins (not counted as a retry).
func (a *App) autoRetryLater(autoID int, subIDs []int, step int) {
	if _, err := a.db.Exec(`UPDATE denma_automation_sends SET status = 'retry', next_at = NOW() + make_interval(mins => $4)
		WHERE automation_id = $1 AND subscriber_id = ANY($2) AND step = $3 AND status = 'queued'`,
		autoID, pq.Array(subIDs), step, denmaAutoUntakenMins); err != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", autoID, err)
	}
}

// errDenmaAutoRestart is the outcome of an e-mail that was being sent when
// the process stopped.
var errDenmaAutoRestart = errors.New("the server stopped while it was being sent")

// autoOutcome records the mail server's answer to e-mail `step` of a run:
// sent (or maybe delivered), and on to the next e-mail, if any; or tried
// again later; or failed.
func (a *App) autoOutcome(autoID, subID, run, step int, did string, err error) {
	guard := []any{autoID, subID, run, step}
	if err == nil || errors.Is(err, errDenmaAutoRestart) || manager.DenmaUncertain(err) {
		status, errText := "sent", ""
		if err != nil {
			status = "uncertain"
			errText = "It may have been delivered: " + err.Error() + " before the mail server's answer. It isn't sent again."
		}
		if _, err := a.db.Exec(`WITH u AS (UPDATE denma_automation_sends d SET
				step = COALESCE(s.step, d.step),
				status = CASE WHEN s.step IS NULL THEN 'sent' ELSE 'waiting' END,
				next_at = CASE WHEN s.step IS NOT NULL THEN NOW() + make_interval(mins => s.delay_minutes) END,
				retries = 0, error = $5, did = $6, sent_at = NOW()
			FROM (SELECT 1) x LEFT JOIN denma_automation_steps s ON s.automation_id = $1 AND s.step = $4 + 1
			WHERE d.automation_id = $1 AND d.subscriber_id = $2 AND d.runs = $3 AND d.step = $4 AND d.status = 'sending' RETURNING 1)
			INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did, error)
			SELECT $1, $2, $3, $4, $7, $6, $5 FROM u`,
			append(guard, errText, did, status)...); err != nil {
			a.log.Printf("denma: error recording automation %d's run: %v", autoID, err)
		}
		return
	}

	// Not sent. Worth trying again (the server said "try later", or
	// couldn't be reached), a few times, or not.
	if manager.DenmaRetryable(err) {
		var status string
		if e := a.db.Get(&status, `UPDATE denma_automation_sends SET
				status = CASE WHEN retries < $5 THEN 'retry' ELSE 'failed' END,
				next_at = CASE WHEN retries < $5 THEN NOW() + make_interval(mins => ($6::INT[])[retries + 1]) END,
				retries = retries + 1, error = $7, sent_at = NOW()
			WHERE automation_id = $1 AND subscriber_id = $2 AND runs = $3 AND step = $4 AND status = 'sending'
			RETURNING status`, append(guard, len(denmaRetryWaits), pq.Array(denmaRetryWaits), err.Error())...); e != nil {
			a.log.Printf("denma: error recording automation %d's run: %v", autoID, e)
			return
		}
		if status == "failed" {
			a.autoLog(autoID, subID, run, step, "failed", did, err.Error())
		}
		return
	}
	if _, e := a.db.Exec(`WITH u AS (UPDATE denma_automation_sends SET status = 'failed', error = $5, next_at = NULL, sent_at = NOW()
			WHERE automation_id = $1 AND subscriber_id = $2 AND runs = $3 AND step = $4 AND status = 'sending' RETURNING 1)
		INSERT INTO denma_automation_log (automation_id, subscriber_id, run, step, status, did, error)
		SELECT $1, $2, $3, $4, 'failed', $6, $5 FROM u`,
		append(guard, err.Error(), did)...); e != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", autoID, e)
	}
}

// runAutoDue sends auto's e-mails that are due, up to limit: follow-ups whose
// wait is over, and e-mails being tried again. Returns how many were handed
// over.
func (a *App) runAutoDue(auto denmaAutomation, limit int, claim, centerClaim int64) int {
	if !auto.SendEmail || limit <= 0 {
		return 0
	}
	var due []struct {
		SubID int    `db:"subscriber_id"`
		Step  int    `db:"step"`
		Run   int    `db:"runs"`
		Did   string `db:"did"`
	}
	if err := a.db.Select(&due, `WITH due AS (
			SELECT automation_id, subscriber_id FROM denma_automation_sends
			WHERE automation_id = $1 AND status IN ('waiting', 'retry') AND next_at <= NOW()
			ORDER BY next_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE denma_automation_sends d SET status = 'queued', sent_at = NOW() FROM due
		WHERE d.automation_id = due.automation_id AND d.subscriber_id = due.subscriber_id
		RETURNING d.subscriber_id, d.step, d.runs, d.did`, auto.ID, limit); err != nil {
		a.log.Printf("denma: error getting automation %d's due e-mails: %v", auto.ID, err)
		return 0
	}
	byStep := map[int][]int{}
	runs, dids := map[int]int{}, map[int]string{}
	for _, d := range due {
		byStep[d.Step] = append(byStep[d.Step], d.SubID)
		runs[d.SubID], dids[d.SubID] = d.Run, d.Did
	}
	steps := make([]int, 0, len(byStep))
	for n := range byStep {
		steps = append(steps, n)
	}
	slices.Sort(steps)

	pushed := 0
	for _, n := range steps {
		ids := byStep[n]
		did := func(id int) string {
			if n == 1 {
				return dids[id]
			}
			return "sent e-mail " + strconv.Itoa(n)
		}
		tplID, subject, ok := auto.stepEmail(n)
		if !ok {
			// The series was shortened since: it ends here.
			if _, err := a.db.Exec(`UPDATE denma_automation_sends SET status = 'sent', next_at = NULL
				WHERE automation_id = $1 AND subscriber_id = ANY($2) AND step = $3 AND status = 'queued'`, auto.ID, pq.Array(ids), n); err != nil {
				a.log.Printf("denma: error recording automation %d's run: %v", auto.ID, err)
			}
			continue
		}

		// Still someone it may e-mail, and (for a follow-up) meeting its
		// conditions.
		var rows []struct {
			ID    int  `db:"id"`
			OK    bool `db:"ok"`
			Meets bool `db:"meets"`
			Quiet bool `db:"quiet"`
		}
		if err := a.db.Select(&rows, `SELECT s.id,
			s.status <> 'blocklisted'
				AND NOT EXISTS (SELECT 1 FROM denma_no_consent nc WHERE nc.email_hash = denma_email_hash(s.email))
				AND EXISTS (SELECT 1 FROM subscriber_lists asl JOIN lists al ON al.id = asl.list_id WHERE asl.subscriber_id = s.id
					AND (asl.status = 'confirmed' OR (asl.status = 'unconfirmed' AND al.optin = 'single'))) AS ok,
			((cardinality($2::TEXT[]) = 0 OR COALESCE(s.attribs->'tags' ?| $2::TEXT[], false))
				AND NOT COALESCE(s.attribs->'tags' ?| $3::TEXT[], false)) AS meets,
			($4::INT = 0 OR NOT `+denmaMessagesWithin("s.id", "$4", "$5")+`) AS quiet
			FROM subscribers s WHERE s.id = ANY($1)`, pq.Array(ids), denmaTextArray(auto.IfTags), denmaTextArray(auto.UnlessTags),
			auto.QuietDays, auto.ID); err != nil {
			a.log.Printf("denma: error getting automation %d's due e-mails: %v", auto.ID, err)
			a.autoRetryLater(auto.ID, ids, n)
			continue
		}
		var send, recent []int
		for _, r := range rows {
			switch {
			case !r.OK:
				a.autoStop(auto.ID, r.ID, runs[r.ID], n, "skipped", fmt.Sprintf("Stopped before e-mail %d: they've unsubscribed or been blocklisted, or are on no list.", n), "")
			case n > 1 && !r.Meets:
				a.autoStop(auto.ID, r.ID, runs[r.ID], n, "skipped", fmt.Sprintf("Stopped before e-mail %d: %s.", n, auto.Conditions()), "")
			case !r.Quiet:
				recent = append(recent, r.ID)
			default:
				send = append(send, r.ID)
			}
		}
		// Those who've had another e-mail lately: this one is skipped, and
		// their series goes on.
		if len(recent) > 0 {
			q := auto.quietSkipped(n)
			a.autoSkipEmail(auto.ID, n, recent, strings.ToUpper(q[:1])+q[1:]+".")
		}
		if len(send) == 0 {
			continue
		}
		tpl, err := a.autoTemplate(tplID, true)
		var subs []models.Subscriber
		if err == nil {
			err = a.db.Select(&subs, `SELECT id, created_at, updated_at, uuid, email, name, first_name, last_name, attribs, status FROM subscribers WHERE id = ANY($1)`, pq.Array(send))
		}
		if err != nil {
			for _, id := range send {
				a.autoStop(auto.ID, id, runs[id], n, "failed", did(id), err.Error())
			}
			continue
		}
		stepAuto := auto
		stepAuto.Subject = subject
		pushed += a.autoSend(stepAuto, n, tpl, subs, runs, did, claim, centerClaim)
	}
	if len(due) > 0 {
		a.log.Printf("denma: automation %q sent %d due e-mails of %d", auto.Name, pushed, len(due))
	}
	return pushed
}

// denmaAutoRecovered holds the centers whose runs left by the last process
// have been seen to (denmaAutoRecover).
var denmaAutoRecovered sync.Map

// denmaProcessStart is when this process started: runs queued or being sent
// before then were left by the last one.
var denmaProcessStart = time.Now()

// denmaAutoStale is how long a run may be queued or being sent: longer, and
// its e-mail was lost on the way (a job that stopped, a write that failed).
const denmaAutoStale = time.Hour

// denmaAutoRecover sees to runs left queued or being sent: by the last
// process (once in this one), or for longer than denmaAutoStale. Queued
// e-mails weren't sent, and go again; those being sent may have been
// delivered.
func (a *App) denmaAutoRecover() {
	before := time.Now().Add(-denmaAutoStale)
	if _, done := denmaAutoRecovered.LoadOrStore(a.ko.String("denma.center"), true); !done {
		before = denmaProcessStart
	}
	if _, err := a.db.Exec(`UPDATE denma_automation_sends SET status = 'retry', next_at = NOW() WHERE status = 'queued' AND sent_at < $1`, before); err != nil {
		a.log.Printf("denma: error seeing to the automations' unsent e-mails: %v", err)
	}
	var left []struct {
		AutoID int    `db:"automation_id"`
		SubID  int    `db:"subscriber_id"`
		Run    int    `db:"runs"`
		Step   int    `db:"step"`
		Did    string `db:"did"`
	}
	if err := a.db.Select(&left, `SELECT automation_id, subscriber_id, runs, step, did FROM denma_automation_sends
		WHERE status = 'sending' AND sent_at < $1`, before); err != nil {
		a.log.Printf("denma: error seeing to the automations' e-mails being sent: %v", err)
		return
	}
	for _, l := range left {
		a.autoOutcome(l.AutoID, l.SubID, l.Run, l.Step, l.Did, errDenmaAutoRestart)
	}
	if len(left) > 0 {
		a.log.Printf("denma: %d automation e-mails were being sent when the server stopped (or for over an hour): marked as maybe delivered", len(left))
	}
}

// denmaAutoStepStat is how one of an automation's e-mails has gone.
type denmaAutoStepStat struct {
	Step    int `db:"step"`
	Sent    int `db:"sent"`
	Failed  int `db:"failed"`
	Skipped int `db:"skipped"`
	Waiting int `db:"waiting"`
}

// autoStepStats is how each of an automation's e-mails has gone.
func (a *App) autoStepStats(id int) []denmaAutoStepStat {
	var out []denmaAutoStepStat
	if err := a.db.Select(&out, `SELECT n.step,
		COUNT(*) FILTER (WHERE g.status IN ('sent', 'uncertain')) AS sent,
		COUNT(*) FILTER (WHERE g.status = 'failed') AS failed,
		COUNT(*) FILTER (WHERE g.status = 'skipped') AS skipped,
		(SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = $1 AND d.step = n.step
			AND d.status IN ('waiting', 'retry', 'queued', 'sending')) AS waiting
		FROM (SELECT 1 AS step UNION SELECT step FROM denma_automation_steps WHERE automation_id = $1) n
		LEFT JOIN denma_automation_log g ON g.automation_id = $1 AND g.step = n.step
		GROUP BY n.step ORDER BY n.step`, id); err != nil {
		a.log.Printf("denma: error getting automation %d's e-mails: %v", id, err)
	}
	return out
}
