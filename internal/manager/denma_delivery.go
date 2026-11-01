package manager

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knadh/listmonk/models"
)

// denma: delivery of a message (msgQ) whose sender must know its fate (cmd/denma_automations.go).

// denmaDeliveryHeader carries a delivery's token to the worker, and is removed before sending.
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
	// denmaInFlight is how many deliveries a worker has taken and not yet finished.
	denmaInFlight atomic.Int64
	// denmaStopping is set by a shutdown: no delivery is taken after.
	denmaStopping atomic.Bool
)

// DenmaStopDeliveries stops deliveries being taken: those waiting are withdrawn unsent.
func DenmaStopDeliveries() { denmaStopping.Store(true) }

// denmaMarkClosed marks msgQ closed once no delivery is being pushed to it.
func (m *Manager) denmaMarkClosed() {
	m.denmaCloseMu.Lock()
	m.denmaClosed = true
	m.denmaCloseMu.Unlock()
}

// denmaMsgWorker serves only msgQ, so arbitrary messages go while campaign workers wait.
func (m *Manager) denmaMsgWorker() {
	for msg := range m.msgQ {
		m.denmaSendMsg(msg)
	}
}

// denmaSendMsg sends an arbitrary message and tells its delivery's sender the outcome.
func (m *Manager) denmaSendMsg(msg models.Message) {
	d, send := denmaTake(msg)
	if !send {
		return
	}
	err := m.messengers[msg.Messenger].Push(msg)
	if err != nil {
		m.log.Printf("error sending a message to subscriber %d: %v", msg.Subscriber.ID, err)
	}
	d.finish(err)
}

// DenmaPushTaken pushes msg and waits up to wait for a worker to take it; reports whether one did.
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

// DenmaPushTracked queues msg as PushMessage does, calling onTaken and onDone as DenmaPushTaken does.
func (m *Manager) DenmaPushTracked(msg models.Message, onTaken func() error, onDone func(error)) error {
	m.denmaCloseMu.RLock()
	defer m.denmaCloseMu.RUnlock()
	if m.denmaClosed || denmaStopping.Load() {
		return errors.New("sending is stopping")
	}
	if msg.Headers == nil {
		msg.Headers = textproto.MIMEHeader{}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	denmaDeliveries.Store(tok, &denmaDelivery{taken: make(chan bool, 1), onTaken: onTaken, onDone: onDone})
	msg.Headers.Set(denmaDeliveryHeader, tok)
	if err := m.PushMessage(msg); err != nil {
		denmaDeliveries.Delete(tok)
		return err
	}
	return nil
}

// denmaTake is the worker's side: whether to send msg, and the delivery to finish.
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

// DenmaDeliveriesWait waits up to wait for the deliveries being sent to finish.
func DenmaDeliveriesWait(wait time.Duration) bool {
	for deadline := time.Now().Add(wait); denmaInFlight.Load() > 0; {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}
