package manager

// denma: where replies to a campaign's e-mails go (denma_reply_to.go).

import (
	"net/textproto"
	"testing"

	"github.com/knadh/listmonk/models"
)

func TestDenmaSetReplyTo(t *testing.T) {
	for _, tc := range []struct {
		name, center, campaign, header, want string
	}{
		{name: "none", want: ""},
		{name: "the center's", center: "office@center.org", want: "office@center.org"},
		{name: "the campaign's over the center's", center: "office@center.org", campaign: "retreats@center.org", want: "retreats@center.org"},
		{name: "the campaign's over a header", campaign: "retreats@center.org", header: "old@center.org", want: "retreats@center.org"},
		{name: "a header over the center's", center: "office@center.org", header: "api@center.org", want: "api@center.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{}
			m.DenmaSetReplyTo(tc.center)
			h := textproto.MIMEHeader{}
			if tc.header != "" {
				h.Add("reply-to", tc.header)
			}
			m.denmaSetReplyTo(&models.Campaign{ReplyTo: tc.campaign}, h)
			if got := h.Get("Reply-To"); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if n := len(h.Values("Reply-To")); tc.want != "" && n != 1 {
				t.Errorf("%d Reply-To headers", n)
			}
		})
	}
}
