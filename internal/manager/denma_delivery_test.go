package manager

import (
	"errors"
	"net/textproto"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
)

func denmaDeliveryMsg() models.Message {
	return models.Message{Messenger: "email", Subject: "x", Headers: textproto.MIMEHeader{}}
}

// A delivery no worker takes is withdrawn: its sender is told it wasn't
// sent, and a worker that gets to it later skips it. One a worker takes
// runs onTaken before the send, and onDone with its outcome, without the
// delivery's header.
func TestDenmaPushTaken(t *testing.T) {
	m := &Manager{msgQ: make(chan models.Message, 10)}
	taken, done := 0, 0
	onTaken := func() error { taken++; return nil }
	onDone := func(error) { done++ }

	if m.DenmaPushTaken(denmaDeliveryMsg(), 20*time.Millisecond, onTaken, onDone) {
		t.Fatal("taken with no worker")
	}
	if _, send := denmaTake(<-m.msgQ); send || taken != 0 {
		t.Fatalf("a withdrawn delivery: send %v, onTaken %d times; want skipped", send, taken)
	}

	var sentHeader string
	go func() {
		msg := <-m.msgQ
		d, send := denmaTake(msg)
		if send {
			sentHeader = msg.Headers.Get(denmaDeliveryHeader)
			d.finish(errors.New("refused"))
		}
	}()
	if !m.DenmaPushTaken(denmaDeliveryMsg(), time.Second, onTaken, onDone) {
		t.Fatal("not taken by a worker")
	}
	if !DenmaDeliveriesWait(time.Second) || taken != 1 || done != 1 || sentHeader != "" {
		t.Fatalf("onTaken %d, onDone %d, header %q; want 1, 1, none", taken, done, sentHeader)
	}

	// onTaken failing (the run changed): not sent, and the sender's told.
	go func() { denmaTake(<-m.msgQ) }()
	if m.DenmaPushTaken(denmaDeliveryMsg(), time.Second, func() error { return errors.New("changed") }, onDone) || done != 1 {
		t.Fatalf("sent after onTaken failed (onDone %d)", done)
	}

	// Other messages go as before.
	if d, send := denmaTake(denmaDeliveryMsg()); d != nil || !send {
		t.Fatal("an ordinary message wasn't sent")
	}
}

// A tracked message is queued without waiting, and its sender hears of it
// once a worker takes it, and of the mail server's answer.
func TestDenmaPushTracked(t *testing.T) {
	m := &Manager{msgQ: make(chan models.Message, 10)}
	var taken, done int
	var got error
	if err := m.DenmaPushTracked(denmaDeliveryMsg(), func() error { taken++; return nil }, func(err error) { done++; got = err }); err != nil {
		t.Fatal(err)
	}
	if taken != 0 || done != 0 {
		t.Fatalf("told before a worker took it: onTaken %d, onDone %d", taken, done)
	}
	msg := <-m.msgQ
	d, send := denmaTake(msg)
	if !send || taken != 1 || msg.Headers.Get(denmaDeliveryHeader) != "" {
		t.Fatalf("taken: send %v, onTaken %d, header %q", send, taken, msg.Headers.Get(denmaDeliveryHeader))
	}
	d.finish(errors.New("try later"))
	if done != 1 || got == nil || got.Error() != "try later" {
		t.Fatalf("onDone %d times with %v, want once with the answer", done, got)
	}

	m.denmaMarkClosed()
	if err := m.DenmaPushTracked(denmaDeliveryMsg(), nil, nil); err == nil {
		t.Fatal("queued after the manager closed")
	}
}
