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

// denma: each center runs its own campaign manager, replaced on a settings save without a process restart.

// SetNotify sets the campaign status notifier (the center's own). Call it before Run().
func (m *Manager) SetNotify(fn func(subject string, data any) error) {
	m.fnNotify = fn
}

// stoppedScans holds the managers whose campaign scanner should exit.
var stoppedScans sync.Map

// StopScanning stops the manager picking up campaigns; queued messages are still sent until Close().
func (m *Manager) StopScanning() {
	stoppedScans.Store(m, struct{}{})
	m.denmaCloseScan() // and a scan in progress makes no pipe after this (denma_scanhold.go)
}

// scanStopped reports (once) whether StopScanning was called.
func (m *Manager) scanStopped() bool {
	_, ok := stoppedScans.LoadAndDelete(m)
	return ok
}

// denmaQueue is a pipe's queued messages; its checkpoint is the highest ID up to which all finished.
type denmaQueue struct {
	mu     sync.Mutex
	queued []int        // queued and not finished, in order
	done   map[int]bool // finished before an earlier one
}

// denmaQueued records a message queued for a subscriber, before it can reach a worker.
func (p *pipe) denmaQueued(subID int) {
	p.denmaQ.mu.Lock()
	p.denmaQ.queued = append(p.denmaQ.queued, subID)
	p.denmaQ.mu.Unlock()
}

// denmaFinished records a message as finished and moves the checkpoint past those finished in order.
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

// denmaNextCampaigns is the campaign scan's NextCampaigns, saving counts and checkpoints.
func (m *Manager) denmaNextCampaigns() ([]*models.Campaign, error) {
	m.denmaCountsMut.Lock()
	defer m.denmaCountsMut.Unlock()
	ids, counts, lastIDs := m.getCurrentCampaigns()
	return m.store.NextCampaigns(ids, counts, lastIDs)
}

// denmaSaveCounts saves a pipe's sent count, checkpoint and those finished past it (denmaLoadAhead).
func (p *pipe) denmaSaveCounts() error {
	p.m.denmaCountsMut.Lock()
	defer p.m.denmaCountsMut.Unlock()
	if err := p.m.store.UpdateCampaignCounts(p.camp.ID, 0, int(p.sent.Swap(0)), int(p.lastID.Load())); err != nil {
		return err
	}
	p.denmaQ.mu.Lock()
	ahead := make([]int, 0, len(p.denmaQ.done))
	for id := range p.denmaQ.done {
		ahead = append(ahead, id)
	}
	p.denmaQ.mu.Unlock()
	return p.m.store.DenmaSaveSentAhead(p.camp.ID, ahead)
}

// denmaLoadAhead reads those earlier pipes finished past the checkpoint, for this pipe to skip.
func (p *pipe) denmaLoadAhead() error {
	ids, err := p.m.store.DenmaSentAhead(p.camp.ID)
	if err != nil {
		return fmt.Errorf("error reading campaign (%s)'s sends past its checkpoint: %v", p.camp.Name, err)
	}
	if len(ids) > 0 {
		p.denmaAhead = make(map[int]bool, len(ids))
		for _, id := range ids {
			p.denmaAhead[id] = true
		}
	}
	return nil
}

// DenmaStop stops for a shutdown, each running campaign saving its progress; false if wait runs out.
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

// DenmaCountWait waits for the daily limit and counts the message (give-back func); false if stopped().
type DenmaCountWait func(stopped func() bool) (bool, func())

// DenmaDailyWait, if set, is DenmaCountWait for the hub's daily limit (cmd/denma_daily.go).
var DenmaDailyWait func(center string, stopped func() bool) (bool, func())

// DenmaPaceWait, if set, blocks a campaign message until its turn at the send rate (cmd/denma_sending.go).
var DenmaPaceWait func(stopped func() bool) bool

// DenmaDailyCounted, DenmaCenterCounted and DenmaPaced mark messages whose waits already ran.
const (
	DenmaDailyCounted  = "X-Denma-Daily-Counted"
	DenmaCenterCounted = "X-Denma-Center-Counted"
	DenmaPaced         = "X-Denma-Paced"
)

// DenmaCampaignTest marks a campaign's test message, for the sent e-mails record (cmd/denma_messages.go).
const DenmaCampaignTest = "X-Denma-Campaign-Test"

// denmaMarkCounted marks a campaign message's headers with what its waits counted, or clears the marks.
func (m *Manager) denmaMarkCounted(h textproto.MIMEHeader, waited bool) {
	if !waited && DenmaDailyWait != nil {
		h.Set(DenmaCampaignTest, "1")
	} else {
		h.Del(DenmaCampaignTest)
	}
	for name, on := range map[string]bool{DenmaDailyCounted: waited && DenmaDailyWait != nil,
		DenmaCenterCounted: waited && m.denmaCenterWait != nil, DenmaPaced: waited && DenmaPaceWait != nil} {
		if on {
			h.Set(name, "1")
		} else {
			h.Del(name)
		}
	}
}

// DenmaSetWindowWait sets the wait for the center's sending hours (cmd/denma_sendwindow.go), before Run.
func (m *Manager) DenmaSetWindowWait(wait func(stopped func() bool) bool) {
	m.denmaWindowWait = wait
}

// DenmaSetCenterWait sets the wait for the center's own daily cap (cmd/denma_daily.go) and its slug.
func (m *Manager) DenmaSetCenterWait(center string, wait DenmaCountWait) {
	m.denmaCenter, m.denmaCenterWait = center, wait
}

// denmaWaits runs a message's waits in order: sending hours, center cap, hub limit, send rate.
func (m *Manager) denmaWaits(stopped, gone func() bool, kept *[]func()) bool {
	for {
		if m.denmaWindowWait != nil && !m.denmaWindowWait(stopped) {
			return false
		}
		var back []func()
		giveBack := func() {
			for _, f := range back {
				f()
			}
		}
		if m.denmaCenterWait != nil {
			ok, f := m.denmaCenterWait(stopped)
			if !ok {
				return false
			}
			back = append(back, f)
		}
		if DenmaDailyWait != nil {
			ok, f := DenmaDailyWait(m.denmaCenter, stopped)
			if !ok {
				giveBack()
				return false
			}
			back = append(back, f)
		}
		if m.denmaWindowWait != nil && !m.denmaWindowWait(func() bool { return true }) { // closed meanwhile
			giveBack()
			continue
		}
		if gone != nil && gone() {
			giveBack()
			return false
		}
		if DenmaPaceWait != nil && !DenmaPaceWait(stopped) {
			giveBack()
			return false
		}
		if kept != nil {
			*kept = back
		}
		return true
	}
}

// denmaGiveBack gives back what a message's waits counted if its send failed before the mail server.
func (m *CampaignMessage) denmaGiveBack(err error) {
	if err == nil || DenmaUncertain(err) {
		return
	}
	for _, f := range m.denmaCounted {
		f()
	}
}

// denmaRecheckAfter is how long after its batch was fetched a message is checked again.
const denmaRecheckAfter = time.Minute

// denmaWaitsFor runs a message's waits and says whether it goes; after a wait its subscriber is rechecked.
func (m *Manager) denmaWaitsFor(msg *CampaignMessage) bool {
	p := msg.pipe
	passed := false
	ok := m.denmaWaits(p.stopped.Load, func() bool {
		if time.Since(time.Unix(0, p.denmaFetchedAt.Load())) < denmaRecheckAfter {
			return false
		}
		still, err := m.store.DenmaStillSent(msg.Campaign.ID, msg.Subscriber.ID)
		if err != nil {
			m.log.Printf("denma: error checking subscriber %d for campaign %s: %v", msg.Subscriber.ID, msg.Campaign.Name, err)
			p.denmaFailed(msg.Subscriber.ID, "Couldn't check that they're still subscribed: "+err.Error(), true, false)
			still = false
		}
		passed = !still
		return passed
	}, &msg.denmaCounted)
	if passed && !msg.denmaRetry {
		p.denmaFinished(msg.Subscriber.ID)
	}
	return ok
}

// Failed sends (cmd/denma_retries.go): each is recorded with its reason and whether to try again.

// denmaFailStreak is how many sends in a row may fail before the campaign is paused.
const denmaFailStreak = 20

// DenmaRetryable reports whether a failed send is worth trying again, for others that send.
func DenmaRetryable(err error) bool { return denmaTemporary(err) }

// denmaTemporary reports whether a failed send is worth trying again.
func denmaTemporary(err error) bool {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code < 500
	}
	return !DenmaUncertain(err)
}

// DenmaUncertain reports whether a failed send may still have been delivered (no answer after writing).
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

// denmaFailed records a failed send, and pauses the campaign after denmaStreakLimit in a row.
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

// denmaPauseReason is why a campaign was paused for errors, for the notification.
func (p *pipe) denmaPauseReason() string {
	if p.denmaStreakHit.Load() {
		return fmt.Sprintf("%d sends in a row failed: the mail server may be down or refusing mail. Resuming the campaign tries again those the server refused; "+
			"those cut off part-way may have been delivered, and are sent again only by \"Resend to them\" once the campaign has finished.",
			p.denmaStreakLimit())
	}
	return "Too many errors"
}

// denmaNextRetries queues the next batch of failed sends to retry, and reports whether there were any.
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

// denmaRetryLater keeps a campaign running if temporary failures are left to retry (cmd/denma_retries.go).
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
