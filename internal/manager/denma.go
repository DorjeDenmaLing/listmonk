package manager

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/knadh/smtppool/v2"
)

// denma: many centers run in one process, each with its own campaign manager,
// and a center's settings save replaces its manager without restarting the
// process (upstream restarts the process, so Close() was enough).

// SetNotify sets the function that sends campaign status notifications (the
// center's own notifier rather than the process-wide one New() uses). Call it
// before Run().
func (m *Manager) SetNotify(fn func(subject string, data any) error) {
	m.fnNotify = fn
}

// stoppedScans holds the managers whose campaign scanner should exit.
var stoppedScans sync.Map

// StopScanning stops the manager picking up campaigns, so that a replacement
// manager doesn't run alongside it. Messages already queued are still sent
// until Close().
func (m *Manager) StopScanning() {
	stoppedScans.Store(m, struct{}{})
}

// scanStopped reports (once) whether StopScanning was called.
func (m *Manager) scanStopped() bool {
	_, ok := stoppedScans.LoadAndDelete(m)
	return ok
}

// denmaQueue is a campaign pipe's queued messages, for its resume checkpoint
// (pipe.lastID, saved as campaigns.last_subscriber_id by the campaign scan
// and when the pipe ends): the highest subscriber ID up to which every queued
// message has finished, sent or failed. Upstream PR #3222 keeps fetching from
// moving the checkpoint, but takes the highest ID sent, while the workers may
// still be sending lower ones (or skip them, once the campaign is paused); a
// restart then skipped those. Messages are queued in ID order and finish in
// any order.
type denmaQueue struct {
	mu     sync.Mutex
	queued []int        // queued and not finished, in order
	done   map[int]bool // finished before an earlier one
}

// denmaQueued records a message queued for a subscriber. Call it before the
// message can reach a worker.
func (p *pipe) denmaQueued(subID int) {
	p.denmaQ.mu.Lock()
	p.denmaQ.queued = append(p.denmaQ.queued, subID)
	p.denmaQ.mu.Unlock()
}

// denmaFinished records a subscriber's message as finished (sent or failed)
// and moves the checkpoint past every message finished in order. A message a
// worker skips (the campaign was stopped) isn't finished, so the checkpoint
// stays before it and a resumed campaign sends it.
func (p *pipe) denmaFinished(subID int) {
	q := &p.denmaQ
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done == nil {
		q.done = map[int]bool{}
	}
	q.done[subID] = true
	for len(q.queued) > 0 && q.done[q.queued[0]] {
		delete(q.done, q.queued[0])
		if id := uint64(q.queued[0]); id > p.lastID.Load() {
			p.lastID.Store(id)
		}
		q.queued = q.queued[1:]
	}
}

// denmaNextCampaigns is the campaign scan's NextCampaigns, which saves the
// running campaigns' sent counts and checkpoints.
func (m *Manager) denmaNextCampaigns() ([]*models.Campaign, error) {
	m.denmaCountsMut.Lock()
	defer m.denmaCountsMut.Unlock()
	ids, counts, lastIDs := m.getCurrentCampaigns()
	return m.store.NextCampaigns(ids, counts, lastIDs)
}

// denmaSaveCounts saves a pipe's sent count since the last save, and its
// checkpoint, when it ends.
func (p *pipe) denmaSaveCounts() error {
	p.m.denmaCountsMut.Lock()
	defer p.m.denmaCountsMut.Unlock()
	return p.m.store.UpdateCampaignCounts(p.camp.ID, 0, int(p.sent.Swap(0)), int(p.lastID.Load()))
}

// DenmaStop stops the manager for a shutdown: it picks up no more campaigns,
// and each running campaign stops where it is and saves its progress (its
// pipe's cleanup: the sent count and the exact checkpoint), keeping its
// status, so that the next start resumes it without sending anyone a second
// copy. It returns once every campaign has saved, or false after wait.
func (m *Manager) DenmaStop(wait time.Duration) bool {
	m.StopScanning()
	for deadline := time.Now().Add(wait); ; {
		m.pipesMut.RLock()
		n := len(m.pipes)
		for _, p := range m.pipes {
			p.Stop(false) // and any a scan started meanwhile
		}
		m.pipesMut.RUnlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// DenmaDailyWait, if set, blocks a campaign message until the hub's daily
// limit lets it go (cmd/denma_daily.go), and returns false if stopped()
// first: its campaign was paused or stopped, so the message is skipped as a
// stopped campaign's are, and sent when it resumes.
var DenmaDailyWait func(stopped func() bool) bool

// DenmaPaceWait, if set, blocks a campaign message until its turn at the
// process's send rate (cmd/denma_sending.go), after the daily waits, and
// returns false if stopped() first, as DenmaDailyWait: a shutdown then
// skips the message instead of waiting for its turn.
var DenmaPaceWait func(stopped func() bool) bool

// DenmaDailyCounted and DenmaCenterCounted mark a campaign message that
// DenmaDailyWait and the center's wait counted as they let it go, so that
// its send doesn't count it again (cmd/denma_sending.go, denma_daily.go);
// DenmaPaced, one that had its turn (DenmaPaceWait), so that it doesn't
// wait for another.
const (
	DenmaDailyCounted  = "X-Denma-Daily-Counted"
	DenmaCenterCounted = "X-Denma-Center-Counted"
	DenmaPaced         = "X-Denma-Paced"
)

// denmaMarkCounted marks a campaign message's headers with what its waits
// counted (waited: whether they ran), and clears the marks otherwise.
func (m *Manager) denmaMarkCounted(h textproto.MIMEHeader, waited bool) {
	for name, on := range map[string]bool{DenmaDailyCounted: waited && DenmaDailyWait != nil,
		DenmaCenterCounted: waited && m.denmaCenterWait != nil, DenmaPaced: waited && DenmaPaceWait != nil} {
		if on {
			h.Set(name, "1")
		} else {
			h.Del(name)
		}
	}
}

// DenmaSetCenterWait sets the wait for the manager's center's own daily cap
// (cmd/denma_daily.go), as DenmaDailyWait. Call it before Run.
func (m *Manager) DenmaSetCenterWait(wait func(stopped func() bool) bool) {
	m.denmaCenterWait = wait
}

// Failed sends (cmd/denma_retries.go). Each is recorded with its reason and
// whether it's worth trying again: the mail server unreachable or answering
// "try later" (a 4xx reply) is temporary; a 5xx reply, or a message that
// couldn't be made from the template, isn't; nor is one that may have been
// delivered all the same (DenmaUncertain), which is never sent again
// automatically. A pipe first sends the
// campaign's failed sends again (all of them when a paused campaign is
// resumed; the temporary ones on a retry run, which does nothing else), and a
// campaign that reaches the end of its list with temporary ones left stays
// running, to be picked up again for them after a wait.

// denmaFailStreak is how many sends in a row may fail before the campaign is
// paused (or listmonk's max_send_errors, if lower; none if that's off): a mail
// server that's down would otherwise fail everyone left.
const denmaFailStreak = 20

// denmaTemporary reports whether a failed send is worth trying again.
func denmaTemporary(err error) bool {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code < 500
	}
	return !DenmaUncertain(err)
}

// DenmaUncertain reports whether a failed send may have been delivered all
// the same: the connection failed without the mail server's answer, which,
// after the message is written, may mean it took it. smtppool sends again
// itself when an earlier step fails (a stale connection), so what's left is
// mostly that; a failed connect, DNS lookup, TLS handshake or wait for a
// connection, or an answer, is certain.
func DenmaUncertain(err error) bool {
	var (
		tp   *textproto.Error
		op   *net.OpError
		dns  *net.DNSError
		ne   net.Error
		cert *tls.CertificateVerificationError
		ua   x509.UnknownAuthorityError
		ae   tls.AlertError
	)
	switch {
	case err == nil, errors.As(err, &tp), errors.As(err, &dns), errors.As(err, &cert), errors.As(err, &ua), errors.As(err, &ae),
		errors.Is(err, smtppool.ErrPoolClosed), strings.Contains(err.Error(), "waiting for free conn"):
		return false
	case errors.As(err, &op) && (op.Op == "dial" || op.Op == "remote error"):
		return false
	}
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// denmaSent records a message's outcome: a failure, or a retry sent.
func (p *pipe) denmaSent(msg CampaignMessage, err error) {
	if err != nil {
		p.denmaFailed(msg.Subscriber.ID, err.Error(), denmaTemporary(err), DenmaUncertain(err))
		return
	}
	p.denmaStreak.Store(0)
	if msg.denmaRetry {
		if err := p.m.store.DenmaRetrySent(p.camp.ID, msg.Subscriber.ID); err != nil {
			p.m.log.Printf("denma: error recording a retry sent (%s, subscriber %d): %v", p.camp.Name, msg.Subscriber.ID, err)
		}
	}
}

// denmaRenderFailed records a subscriber whose message couldn't be made.
func (p *pipe) denmaRenderFailed(s models.Subscriber, err error) {
	p.denmaFailed(s.ID, "The message couldn't be made from the campaign's template: "+err.Error(), false, false)
}

// denmaFailed records a failed send, and pauses the campaign after
// denmaStreakLimit in a row.
func (p *pipe) denmaFailed(subID int, reason string, temporary, uncertain bool) {
	if err := p.m.store.DenmaSendFailed(p.camp.ID, subID, reason, temporary, uncertain); err != nil {
		p.m.log.Printf("denma: error recording a failed send (%s, subscriber %d): %v", p.camp.Name, subID, err)
	}
	n := p.denmaStreak.Add(1)
	if limit := p.denmaStreakLimit(); limit > 0 && n >= int64(limit) && !p.stopped.Load() {
		p.denmaStreakHit.Store(true)
		p.Stop(true)
		p.m.log.Printf("denma: %d sends in a row failed, pausing campaign %s", n, p.camp.Name)
	}
}

func (p *pipe) denmaStreakLimit() int {
	if p.m.cfg.MaxSendErrors < 1 {
		return 0
	}
	return min(denmaFailStreak, p.m.cfg.MaxSendErrors)
}

// denmaPauseReason is why a campaign was paused for errors, for the
// notification.
func (p *pipe) denmaPauseReason() string {
	if p.denmaStreakHit.Load() {
		return fmt.Sprintf("%d sends in a row failed: the mail server may be down or refusing mail. Resuming the campaign tries them again first.",
			p.denmaStreakLimit())
	}
	return "Too many errors"
}

// denmaNextRetries queues the next batch of the campaign's failed sends to
// try again, and reports whether there were any.
func (p *pipe) denmaNextRetries() (bool, error) {
	subs, err := p.m.store.DenmaRetrySubscribers(p.camp.ID, p.denmaRetryCursor, p.m.cfg.BatchSize, !p.denmaRetryOnly)
	if err != nil {
		return false, fmt.Errorf("error fetching the failed sends to try again (%s): %v", p.camp.Name, err)
	}
	if len(subs) == 0 {
		p.denmaRetrying.Store(false)
		return false, nil
	}
	p.denmaRetryCursor = subs[len(subs)-1].ID
	if p.denmaRetried == nil {
		p.denmaRetried = map[int]bool{}
	}
	p.m.log.Printf("denma: trying %d failed sends again (%s)", len(subs), p.camp.Name)
	for _, s := range subs {
		// (The rest of the campaign skips them.)
		p.denmaRetried[s.ID] = true
		msg, err := p.newMessage(s)
		if err != nil {
			p.denmaRenderFailed(s, err)
			continue
		}
		msg.denmaRetry = true
		p.m.campMsgQ <- msg
	}
	return true, nil
}

// denmaRetryLater keeps a campaign that reached the end of its list running
// if it has temporary failures left to try again (cmd/denma_retries.go sets
// when), and reports whether it did.
func (p *pipe) denmaRetryLater() bool {
	later, err := p.m.store.DenmaRetryLater(p.camp.ID)
	if err != nil {
		p.m.log.Printf("denma: error checking campaign (%s)'s failed sends: %v", p.camp.Name, err)
		return false
	}
	if later {
		p.m.log.Printf("denma: campaign (%s) has failed sends to try again later", p.camp.Name)
	}
	return later
}
