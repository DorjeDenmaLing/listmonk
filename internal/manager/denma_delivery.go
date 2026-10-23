package manager

import (
	"crypto/rand"
	"encoding/hex"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knadh/listmonk/models"
)

// denma: delivery of an arbitrary message (msgQ) that its sender must know
// the fate of: automations' e-mails (cmd/denma_automations.go). PushMessage
// only queues a message, so a full queue, a restart or the mail server's
// answer was lost to the sender, who recorded it as sent. DenmaPushTaken
// returns only once a worker has taken the message, after its onTaken (the
// sender records it as being sent, before the mail server sees it), or once
// it's been withdrawn, untaken: then it certainly wasn't sent. The worker
// calls onDone with the mail server's answer.

// denmaDeliveryHeader carries a delivery's token to the worker; it's taken off
// before the message is sent.
const denmaDeliveryHeader = "X-Denma-Delivery"

// Delivery states.
const (
	denmaQueued int32 = iota
	denmaTaken
	denmaWithdrawn
)

type denmaDelivery struct {
	state   atomic.Int32
	taken   chan bool
	onTaken func() error
	onDone  func(error)
}

var (
	denmaDeliveries sync.Map // token -> *denmaDelivery
	// denmaInFlight is how many deliveries a worker has taken and not yet
	// finished, for a shutdown to wait for (DenmaDeliveriesWait).
	denmaInFlight atomic.Int64
	// denmaStopping is set by a shutdown: no delivery is taken after.
	denmaStopping atomic.Bool
)

// DenmaStopDeliveries stops deliveries being taken, for a shutdown: those
// waiting are withdrawn, unsent, and their senders told.
func DenmaStopDeliveries() { denmaStopping.Store(true) }

// denmaMarkClosed marks msgQ closed (Close), once no delivery is being pushed
// to it.
func (m *Manager) denmaMarkClosed() {
	m.denmaCloseMu.Lock()
	m.denmaClosed = true
	m.denmaCloseMu.Unlock()
}

// denmaMsgWorker serves only msgQ, so that arbitrary messages (opt-ins,
// transactional e-mails, automations') go while every worker waits with a
// campaign message (its center's sending hours, the daily limit).
func (m *Manager) denmaMsgWorker() {
	for msg := range m.msgQ {
		m.denmaSendMsg(msg)
	}
}

// denmaSendMsg sends an arbitrary message, telling its delivery's sender, if
// any, how it went.
func (m *Manager) denmaSendMsg(msg models.Message) {
	d, send := denmaTake(msg)
	if !send {
		return
	}
	err := m.messengers[msg.Messenger].Push(msg)
	if err != nil {
		m.log.Printf("error sending message '%s': %v", msg.Subject, err)
	}
	d.finish(err)
}

// DenmaPushTaken pushes msg and waits up to wait for a worker to take it,
// and reports whether one did: then onTaken has run without an error, and
// onDone is called with the send's outcome. If not, the message wasn't sent
// and won't be.
func (m *Manager) DenmaPushTaken(msg models.Message, wait time.Duration, onTaken func() error, onDone func(error)) bool {
	m.denmaCloseMu.RLock()
	defer m.denmaCloseMu.RUnlock()
	if m.denmaClosed || denmaStopping.Load() {
		return false
	}
	if msg.Headers == nil {
		msg.Headers = textproto.MIMEHeader{}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	d := &denmaDelivery{taken: make(chan bool, 1), onTaken: onTaken, onDone: onDone}
	denmaDeliveries.Store(tok, d)
	msg.Headers.Set(denmaDeliveryHeader, tok)

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case m.msgQ <- msg:
	case <-t.C:
		denmaDeliveries.Delete(tok)
		return false
	}
	select {
	case ok := <-d.taken:
		return ok
	case <-t.C:
		if d.state.CompareAndSwap(denmaQueued, denmaWithdrawn) {
			denmaDeliveries.Delete(tok)
			return false
		}
		return <-d.taken // a worker took it just then
	}
}

// denmaTake is the worker's side: for a delivery's message, whether to send
// it (it wasn't withdrawn, and its onTaken went), and the delivery to finish
// (nil for other messages).
func denmaTake(msg models.Message) (*denmaDelivery, bool) {
	tok := msg.Headers.Get(denmaDeliveryHeader)
	if tok == "" {
		return nil, true
	}
	msg.Headers.Del(denmaDeliveryHeader)
	v, ok := denmaDeliveries.LoadAndDelete(tok)
	if !ok {
		return nil, false
	}
	d := v.(*denmaDelivery)
	if denmaStopping.Load() {
		if d.state.CompareAndSwap(denmaQueued, denmaWithdrawn) {
			d.taken <- false
		}
		return nil, false
	}
	if !d.state.CompareAndSwap(denmaQueued, denmaTaken) {
		return nil, false
	}
	if err := d.onTaken(); err != nil {
		d.taken <- false
		return nil, false
	}
	denmaInFlight.Add(1)
	d.taken <- true
	return d, true
}

// finish gives the delivery's sender the send's outcome.
func (d *denmaDelivery) finish(err error) {
	if d == nil {
		return
	}
	defer denmaInFlight.Add(-1)
	d.onDone(err)
}

// DenmaDeliveriesWait waits up to wait for the deliveries being sent to
// finish, for a shutdown, and reports whether they did.
func DenmaDeliveriesWait(wait time.Duration) bool {
	for deadline := time.Now().Add(wait); denmaInFlight.Load() > 0; {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}
