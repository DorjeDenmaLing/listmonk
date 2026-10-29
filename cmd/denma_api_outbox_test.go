package main

import (
	"errors"
	"io"
	"net"
	"net/textproto"
	"testing"

	"github.com/knadh/listmonk/models"
)

// What becomes of an API e-mail by the mail server's answer: temporary
// refusals are tried again, a few times; those that may have been delivered
// never are.
func TestDenmaAPIVerdict(t *testing.T) {
	last := len(denmaRetryWaits)
	dial := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	for _, c := range []struct {
		name    string
		err     error
		retries int
		want    string
		reason  string
	}{
		{"sent", nil, 0, denmaAPISent, ""},
		{"try later", &textproto.Error{Code: 454, Msg: "Throttling failure"}, 0, denmaAPIRetry, ""},
		{"try later, the last time", &textproto.Error{Code: 454}, last - 1, denmaAPIRetry, ""},
		{"try later, out of tries", &textproto.Error{Code: 454}, last, denmaAPIFailed, denmaAPIMsgBusy()},
		{"unreachable", dial, 1, denmaAPIRetry, ""},
		{"unreachable, out of tries", dial, last, denmaAPIFailed, denmaAPIMsgBusy()},
		{"refused", &textproto.Error{Code: 550, Msg: "mailbox unavailable"}, 0, denmaAPIFailed, denmaAPIMsgRefused},
		{"cut off", io.ErrUnexpectedEOF, 0, denmaAPIUncertain, denmaAPIMsgMaybe},
		{"cut off, not tried again however few tries", io.EOF, last, denmaAPIUncertain, denmaAPIMsgMaybe},
		{"the server stopped", errDenmaAPIRestart, 0, denmaAPIUncertain, denmaAPIMsgMaybe},
	} {
		got, reason := denmaAPIVerdict(c.err, c.retries)
		if got != c.want || reason != c.reason {
			t.Errorf("%s: %s %q, want %s %q", c.name, got, reason, c.want, c.reason)
		}
	}
	// An event's reason never has the mail server's words.
	if _, r := denmaAPIVerdict(&textproto.Error{Code: 550, Msg: "mailbox unavailable"}, 0); r == "" || r == "mailbox unavailable" {
		t.Errorf("reason %q", r)
	}
}

// A message built for the outbox has the subscriber's UUID header and, if
// given, the Reply-To, as the first send's does.
func TestDenmaAPIMessage(t *testing.T) {
	var sub models.Subscriber
	sub.UUID, sub.Email = "u-1", "a@example.org"
	m := denmaAPIMessage(sub, "from@b.org", "r@b.org", "Hi", "html", []byte("x"), "y", nil)
	if m.Headers.Get(models.EmailHeaderSubscriberUUID) != "u-1" || m.Headers.Get("Reply-To") != "r@b.org" || m.To[0] != "a@example.org" {
		t.Errorf("headers %v, to %v", m.Headers, m.To)
	}
	if m := denmaAPIMessage(sub, "from@b.org", "", "Hi", "html", nil, "", nil); m.Headers.Get("Reply-To") != "" {
		t.Errorf("a Reply-To with none given: %v", m.Headers)
	}
}
