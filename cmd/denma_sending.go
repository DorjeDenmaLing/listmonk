package main

// denma: one send limit for all centers. Each center has its own campaign
// manager, which keeps listmonk's limits (concurrency × message rate per
// second, and the sliding window) for itself; with hundreds of centers,
// those alone would let them send together at hundreds of times the rate the
// mail provider allows (SES's sending rate and daily quota are the account's).
// So the hub's sending settings, which every center shares, are also the
// limit for everything the process sends, all centers and messengers
// together: never more than concurrency × message rate messages in any one
// second.
//
// listmonk's sliding window isn't used with several centers (each center's
// manager would keep its own, in memory, and here it would hold up password
// resets and opt-ins with campaigns); the hub's daily limit replaces it.
//
// E-mail waits in the SMTP messenger itself (email.BeforePush), so that
// notifications, opt-in confirmations, password resets and invites, which
// don't go through the campaign manager, count too; other messengers
// (postbacks) wait in a wrapper given to the managers. A center sending alone
// still gets the full rate; several share it, in the order they asked.
//
// Every e-mail is also counted for the hub's daily limit (cmd/denma_daily.go),
// which campaign messages wait for.
//
// Multi-center mode only.

import (
	"sync"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/models"
)

// denmaPacer gives each message a time to go: no earlier than the one before
// it, at least a second and denmaSendMargin after the rate-th one before it
// (so never more than rate in any second, while a second's worth may go at
// once, as listmonk's managers send them).
type denmaPacer struct {
	mu    sync.Mutex
	rate  int
	slots []time.Time // the last rate messages' times, oldest first
}

// denmaSendMargin is added to the second, so that messages that reach the
// provider up to this much later or sooner than they left still never make
// more than rate in a second there.
const denmaSendMargin = 100 * time.Millisecond

// denmaSendPacer is the process's one limit.
var (
	denmaSendPacer = &denmaPacer{}
	denmaHookEmail sync.Once
)

// wait blocks until it's this message's turn.
func (p *denmaPacer) wait() {
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

	if d := slot.Sub(now); d > 0 {
		time.Sleep(d)
	}
}

// configure sets the limit to ko's: listmonk's for one install (concurrency
// × message rate per second).
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

// denmaPacedMessenger is a (non e-mail) messenger whose messages wait for the
// shared limit.
type denmaPacedMessenger struct {
	manager.Messenger
}

func (m denmaPacedMessenger) Push(msg models.Message) error {
	denmaSendPacer.wait()
	return m.Messenger.Push(msg)
}

// denmaLimitSending puts an app's sending under the shared limit, which it
// sets to the app's settings (the hub's, which every center shares; the
// latest app to load sets them, as a settings save reloads the hub and then
// each center). It returns the messengers for the campaign manager.
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
		email.BeforePush = func() {
			denmaSendPacer.wait()
			denmaDaily.Add()
		}
		manager.DenmaDailyWait = denmaDaily.Wait
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
