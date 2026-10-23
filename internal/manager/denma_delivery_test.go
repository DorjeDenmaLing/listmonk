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
