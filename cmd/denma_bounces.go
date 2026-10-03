package main

// denma: bounces for all centers, taken by the hub. Every center sends
// through the same mail provider (the hub's settings), which reports bounces
// to one place: one SES/SNS topic, webhook URL or bounce mailbox. So only the
// hub has a bounce manager (cmd/main.go): it receives the webhooks, at the
// same /webhooks/service/<service> as a single install (a center's
// /c/<slug>/webhooks/service/… is passed to it too), and is the only one to
// scan the mailbox. Each bounce is recorded in the center whose mail it was:
//
//  1. the center with the bounce's campaign (its X-Listmonk-Campaign header;
//     SES includes headers only if the identity's notifications are set to
//     include the original headers);
//  2. else the one with its subscriber (X-Listmonk-Subscriber, from mailbox
//     bounces);
//  3. else the only center with the address.
//
// An address in several centers with neither header can't be placed: a hard
// bounce goes on the shared blocklist (the address can't receive mail from
// any center, cmd/denma_blocklist.go); a soft bounce or a complaint is only
// logged, as it may concern one center's mail only.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gofrs/uuid/v5"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

// denmaBounceCB is the hub's bounce recorder: its own (own) on a single
// install, else the router.
func denmaBounceCB(own func(models.Bounce) error, ko *koanf.Koanf) func(models.Bounce) error {
	if !ko.Bool("denma.multi_center") {
		return own
	}
	return func(b models.Bounce) error {
		if denmaHub == nil {
			return fmt.Errorf("bounce for %s before the centers started", b.Email)
		}
		return denmaHub.routeBounce(b)
	}
}

// denmaBounceCampaigns caches which center each bounced campaign is in.
var denmaBounceCampaigns sync.Map // campaign UUID -> slug

// routeBounce records a bounce in its center.
func (d *denmaCenters) routeBounce(b models.Bounce) error {
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	slugs, err := d.bounceCenters(b)
	if err != nil {
		lo.Printf("denma: error finding the center for a bounce (%s, %s): %v", b.Email, b.Type, err)
		return err
	}
	switch {
	case len(slugs) == 1:
		ctr := d.get(slugs[0])
		if ctr == nil {
			lo.Printf("denma: %s bounce for %s in center %s, which isn't running: not recorded", b.Type, b.Email, slugs[0])
			return nil
		}
		return ctr.app.core.RecordBounce(b)
	case len(slugs) == 0:
		lo.Printf("denma: %s bounce for %s, which no center has: not recorded", b.Type, b.Email)
	case b.Type == models.BounceTypeHard:
		lo.Printf("denma: hard bounce for %s, which %d centers have (no campaign or subscriber to tell whose mail it was): blocklisted in all", b.Email, len(slugs))
		denmaBlockEverywhere(b.Email, "hard bounce", "")
	default:
		lo.Printf("denma: %s bounce for %s, which %d centers have (no campaign or subscriber to tell whose mail it was): not recorded", b.Type, b.Email, len(slugs))
	}
	return nil
}

// bounceCenters returns the slugs of the centers the bounce may be for: the
// one with its campaign, else its subscriber, else every one with its address.
func (d *denmaCenters) bounceCenters(b models.Bounce) ([]string, error) {
	if id, err := uuid.FromString(b.CampaignUUID); err == nil {
		if s, ok := denmaBounceCampaigns.Load(id.String()); ok {
			return []string{s.(string)}, nil
		}
		slugs, err := d.findInCenters(`SELECT %s FROM %s.campaigns WHERE uuid = $1::UUID`, id.String())
		if err != nil || len(slugs) == 1 {
			if len(slugs) == 1 {
				denmaBounceCampaigns.Store(id.String(), slugs[0])
			}
			return slugs, err
		}
	}
	if id, err := uuid.FromString(b.SubscriberUUID); err == nil {
		slugs, err := d.findInCenters(`SELECT %s FROM %s.subscribers WHERE uuid = $1::UUID`, id.String())
		if err != nil || len(slugs) == 1 {
			return slugs, err
		}
	}
	if b.Email == "" {
		return nil, nil
	}
	return d.findInCenters(`SELECT %s FROM %s.subscribers WHERE LOWER(email) = $1`, b.Email)
}

// findInCenters runs a query (with %s for the slug literal and the schema)
// in every registered center at once, running or not, and returns the slugs
// of those where it finds a row.
func (d *denmaCenters) findInCenters(q, arg string) ([]string, error) {
	db := d.current().db
	var cs []struct {
		Slug   string `db:"slug"`
		Schema string `db:"schema_name"`
	}
	// Only schemas with listmonk's tables (a center that failed to install
	// may have none).
	if err := db.Select(&cs, `SELECT slug, schema_name FROM denma.centers
		WHERE to_regclass(quote_ident(schema_name) || '.campaigns') IS NOT NULL
		AND to_regclass(quote_ident(schema_name) || '.subscribers') IS NOT NULL ORDER BY id`); err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = "(" + fmt.Sprintf(q, pq.QuoteLiteral(c.Slug), pq.QuoteIdentifier(c.Schema)) + " LIMIT 1)"
	}
	var slugs []string
	if err := db.Select(&slugs, strings.Join(parts, " UNION ALL "), arg); err != nil {
		return nil, err
	}
	return slugs, nil
}
