package main

// denma: one send limit for all centers, set by the hub's sending settings (multi-center mode only).

import (
	"net/http"
	"net/textproto"
	"strconv"
	"sync"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaPacer gives each message a time to go, never more than rate in any second.
type denmaPacer struct {
	mu    sync.Mutex
	rate  int
	slots []time.Time // the last rate messages' times, oldest first
}

// denmaSendMargin is added to the second, to allow for delivery jitter.
const denmaSendMargin = 100 * time.Millisecond

// denmaSendPacer is the process's one limit.
var (
	denmaSendPacer = &denmaPacer{}
	denmaHookEmail sync.Once
)

// wait blocks until it's this message's turn.
func (p *denmaPacer) wait() {
	p.waitUnless(func() bool { return false })
}

// waitUnless is wait, giving up (false) once stopped(); the turn it took is unused.
func (p *denmaPacer) waitUnless(stopped func() bool) bool {
	p.mu.Lock()
	now := time.Now()
	slot := now
	if n := len(p.slots); n > 0 {
		if last := p.slots[n-1]; last.After(slot) {
			slot = last
		}
		if n == p.rate {
			if t := p.slots[0].Add(time.Second + denmaSendMargin); t.After(slot) {
				slot = t
			}
		}
	}
	if len(p.slots) == p.rate {
		p.slots = append(p.slots[:0], p.slots[1:]...)
	}
	p.slots = append(p.slots, slot)
	p.mu.Unlock()

	for {
		d := time.Until(slot)
		if d <= 0 {
			return true
		}
		if stopped() {
			return false
		}
		time.Sleep(min(d, 50*time.Millisecond))
	}
}

// configure sets the limit to ko's: concurrency × message rate per second.
func (p *denmaPacer) configure(ko *koanf.Koanf) {
	rate := max(ko.Int("app.concurrency"), 1) * max(ko.Int("app.message_rate"), 1)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rate != rate {
		// Keep the latest messages' times, so a new rate counts them.
		if len(p.slots) > rate {
			p.slots = append([]time.Time(nil), p.slots[len(p.slots)-rate:]...)
		}
		p.rate = rate
	}
}

// denmaClaimHeader marks an e-mail as claimed against the daily limit, with the claim's ID.
const denmaClaimHeader = "X-Denma-Daily-Claim"

// errDenmaDaily is the daily limit reached for what can be claimed.
var errDenmaDaily = echo.NewHTTPError(http.StatusTooManyRequests,
	"Sending has reached its daily limit (what's kept for confirmations and password resets aside). Try again later.")

// denmaClaimDaily claims n transactional e-mails of the daily limit, short of the reserve; all or none.
func denmaClaimDaily(n int) (claim int64, err error) {
	if n <= 0 {
		return 0, nil
	}
	id, ok := denmaDaily.ClaimAll(n)
	if !ok {
		return 0, errDenmaDaily
	}
	return id, nil
}

// denmaMarkClaim marks an e-mail with its claim (header, claim ID), if any.
func denmaMarkClaim(m *models.Message, header string, claim int64) {
	if claim == 0 {
		return
	}
	if m.Headers == nil {
		m.Headers = textproto.MIMEHeader{}
	}
	m.Headers.Set(header, strconv.FormatInt(claim, 10))
}

// denmaClaimOf takes an e-mail's claim (header) off it, returning its ID.
func denmaClaimOf(m models.Message, header string) (int64, bool) {
	if m.Headers == nil {
		return 0, false
	}
	v := m.Headers.Get(header)
	if v == "" {
		return 0, false
	}
	m.Headers.Del(header)
	id, _ := strconv.ParseInt(v, 10, 64)
	return id, true
}

// denmaClaimTx claims the admin's transactional e-mail (/api/tx); release gives the claim back.
func denmaClaimTx(m *models.Message) (release func(), err error) {
	claim, err := denmaClaimDaily(1)
	if err != nil {
		return nil, err
	}
	denmaMarkClaim(m, denmaClaimHeader, claim)
	return func() { denmaDaily.Release(claim, 1) }, nil
}

// denmaPacedMessenger is a (non e-mail) messenger whose messages wait for the shared limit.
type denmaPacedMessenger struct {
	manager.Messenger
}

func (m denmaPacedMessenger) Push(msg models.Message) error {
	if msg.Headers == nil || msg.Headers.Get(manager.DenmaPaced) == "" {
		denmaSendPacer.wait()
	}
	if msg.Headers != nil {
		msg.Headers.Del(manager.DenmaPaced)
		msg.Headers.Del(manager.DenmaDailyCounted) // counted by its wait
	}
	return m.Messenger.Push(msg)
}

// denmaLimitSending puts an app's sending under the shared limit and returns the messengers.
func denmaLimitSending(msgrs []manager.Messenger, ko *koanf.Koanf) []manager.Messenger {
	if !ko.Bool("denma.multi_center") {
		return msgrs
	}
	denmaSendPacer.configure(ko)
	if ko.String("denma.center") == "" {
		denmaDaily.SetLimit(ko.Int("denma.daily_limit"), ko.Int("denma.daily_reserve")) // the hub's settings
	}
	denmaHookEmail.Do(func() {
		email.DenmaDropSenderHeaders = true // cmd/denma_domains.go, denmaCheckHeaders
		email.BeforePush = func(m models.Message) {
			// A campaign's e-mail waited in the manager (manager.DenmaPaceWait).
			if m.Headers != nil && m.Headers.Get(manager.DenmaPaced) != "" {
				m.Headers.Del(manager.DenmaPaced)
			} else {
				denmaSendPacer.wait()
			}
			// A campaign's e-mail, counted by its wait (denmaDaily.Wait).
			if m.Headers != nil && m.Headers.Get(manager.DenmaDailyCounted) != "" {
				m.Headers.Del(manager.DenmaDailyCounted)
				return
			}
			// An automation's or transactional e-mail, claimed already.
			if claim, ok := denmaClaimOf(m, denmaClaimHeader); ok {
				denmaDaily.AddClaimed(claim)
				return
			}
			denmaDaily.Add()
		}
		manager.DenmaDailyWait = denmaDaily.Wait
		manager.DenmaPaceWait = denmaSendPacer.waitUnless
	})

	out := make([]manager.Messenger, len(msgrs))
	for i, m := range msgrs {
		if _, ok := m.(*email.Emailer); ok {
			out[i] = m // waits in Push
			continue
		}
		out[i] = denmaPacedMessenger{Messenger: m}
	}
	return out
}
