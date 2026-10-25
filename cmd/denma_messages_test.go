package main

import (
	"net/textproto"
	"testing"

	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/models"
)

// What an e-mail was is told by its campaign (or test) and its automation
// header.
func TestDenmaMessageKind(t *testing.T) {
	h := func(kv ...string) textproto.MIMEHeader {
		out := textproto.MIMEHeader{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	camp := &models.Campaign{}
	camp.ID = 3
	auto := models.Message{Headers: textproto.MIMEHeader{}}
	denmaSentAs(&auto, 7, 2)
	for _, c := range []struct {
		m    models.Message
		kind string
		id   int
	}{
		{models.Message{Campaign: camp, Headers: h()}, denmaMsgCampaign, 3},
		{models.Message{Campaign: camp, Headers: h(manager.DenmaCampaignTest, "1")}, denmaMsgTest, 3},
		{auto, denmaMsgAutomation, 7},
		{models.Message{Headers: h(denmaSentAsHeader, "automation 0 1")}, denmaMsgTest, 0},
		{models.Message{Subject: "Confirm"}, denmaMsgOther, 0},
	} {
		kind, camp, au, _ := denmaMessageKind(c.m)
		id := int(camp.Int + au.Int)
		if kind != c.kind || id != c.id {
			t.Errorf("%+v: %s %d, want %s %d", c.m.Headers, kind, id, c.kind, c.id)
		}
		if denmaIsMarketing(c.m) != (c.kind != denmaMsgOther) {
			t.Errorf("%+v: marketing %v", c.m.Headers, denmaIsMarketing(c.m))
		}
	}
}
