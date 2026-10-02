package main

// denma: e-mail domain checks. A subscriber whose address's domain can't
// receive mail (it doesn't exist, has no mail server, or says it takes no
// mail with a "null MX") is blocklisted, with the reason and time in its
// attributes (auto_blocklist_reason, auto_blocklist_time), and is never sent
// an opt-in.
//
//   - Opt-ins: listmonk's opt-in sender (makeOptinNotifyHook) is wrapped
//     (denmaCheckOptin, in buildApp), so the check runs before every opt-in
//     and every sign-up that could send one: the public form and API, adding
//     or editing a subscriber unconfirmed, and "Send opt-in e-mail". A failed
//     public sign-up is told why, so a typo can be fixed.
//   - Everything else (subscribers added already confirmed, imports): a cron
//     job checks subscribers created or changed in the last day, every
//     minute (denmaStartEmailChecks).
//
// Only definite answers count: when DNS times out or fails, the address is
// let through (and the cron job tries again). Domains in
// denma.email_check_skip are never checked (dev: example.com).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const (
	// denmaDNSTimeout is the longest a domain's lookups take.
	denmaDNSTimeout = 5 * time.Second
	// denmaDomainsPerRun is the most new domains the cron job looks up a minute.
	denmaDomainsPerRun = 100
)

// denmaDomainVerdict is what a domain's lookup found.
type denmaDomainVerdict struct {
	bad    bool   // definitely takes no mail
	reason string // why, when bad
	at     time.Time
}

// denmaDomains caches lookups, for every center: good for 6 hours, bad for 1.
var denmaDomains = struct {
	sync.Mutex
	m map[string]denmaDomainVerdict
}{m: map[string]denmaDomainVerdict{}}

// denmaCheckDomain reports whether a domain definitely can't receive mail
// (bad, and why); known is false when DNS gave no definite answer.
func denmaCheckDomain(domain string) (bad bool, reason string, known bool) {
	denmaDomains.Lock()
	v, ok := denmaDomains.m[domain]
	denmaDomains.Unlock()
	ttl := 6 * time.Hour
	if v.bad {
		ttl = time.Hour
	}
	if ok && time.Since(v.at) < ttl {
		return v.bad, v.reason, true
	}

	bad, reason, known = denmaLookupDomain(domain)
	if known {
		denmaDomains.Lock()
		denmaDomains.m[domain] = denmaDomainVerdict{bad: bad, reason: reason, at: time.Now()}
		denmaDomains.Unlock()
	}
	return bad, reason, known
}

// denmaLookupDomain looks a domain's mail servers up: its MX records, or
// without any, its own address (A/AAAA), as mail servers do (RFC 5321).
func denmaLookupDomain(domain string) (bad bool, reason string, known bool) {
	if strings.HasPrefix(domain, "[") {
		return false, "", true // an IP address literal
	}
	ctx, cancel := context.WithTimeout(context.Background(), denmaDNSTimeout)
	defer cancel()
	r := net.DefaultResolver

	notFound := func(err error) bool {
		var de *net.DNSError
		return errors.As(err, &de) && de.IsNotFound
	}

	mxs, err := r.LookupMX(ctx, domain)
	if len(mxs) > 0 {
		// A "null MX" (RFC 7505): the domain says it takes no mail.
		if len(mxs) == 1 && strings.TrimSuffix(mxs[0].Host, ".") == "" {
			return true, fmt.Sprintf("%s doesn't accept e-mail", domain), true
		}
		return false, "", true
	}
	if err != nil && !notFound(err) {
		return false, "", false
	}

	addrs, err := r.LookupHost(ctx, domain)
	if len(addrs) > 0 {
		return false, "", true
	}
	if err != nil && !notFound(err) {
		return false, "", false
	}
	return true, fmt.Sprintf("%s has no mail server", domain), true
}

// denmaEmailSkip is the domains never checked (denma.email_check_skip: a list,
// or a comma-separated string from the environment).
func denmaEmailSkip(ko *koanf.Koanf) map[string]bool {
	out := map[string]bool{}
	for _, v := range append(ko.Strings("denma.email_check_skip"), ko.String("denma.email_check_skip")) {
		for _, d := range strings.Split(v, ",") {
			if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
				out[d] = true
			}
		}
	}
	return out
}

// denmaEmailBad reports whether an address's domain definitely can't receive
// mail, and why.
func denmaEmailBad(email string, skip map[string]bool) (bool, string) {
	i := strings.LastIndex(email, "@")
	if i < 0 {
		return false, ""
	}
	domain := strings.ToLower(email[i+1:])
	if skip[domain] {
		return false, ""
	}
	bad, reason, _ := denmaCheckDomain(domain)
	return bad, reason
}

// denmaBlocklist blocklists a subscriber, as listmonk does (unsubscribing
// them from their lists), recording why in their attributes.
func denmaBlocklist(db *sqlx.DB, email, reason string) {
	if _, err := db.Exec(denmaBlocklistSQL(""), email, denmaReasonAttr(reason)); err != nil {
		lo.Printf("denma: error blocklisting %s: %v", email, err)
	}
}

// denmaBlocklistSQL blocklists the subscriber with an address ($1), as
// listmonk does (unsubscribing them from their lists), with the reason ($2,
// JSON attributes). schema is "" for the connection's own, or `"name".`.
func denmaBlocklistSQL(schema string) string {
	return strings.NewReplacer("S.", schema).Replace(`WITH s AS (
			UPDATE S.subscribers SET status = 'blocklisted', updated_at = NOW(),
				attribs = (CASE WHEN JSONB_TYPEOF(attribs) = 'object' THEN attribs ELSE '{}' END) || $2::JSONB
			WHERE LOWER(email) = LOWER($1) AND status <> 'blocklisted' RETURNING id
		)
		UPDATE S.subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
		WHERE subscriber_id IN (SELECT id FROM s) AND status <> 'unsubscribed'`)
}

// denmaReasonAttr is the attributes recording why and when a subscriber was
// blocklisted.
func denmaReasonAttr(reason string) string {
	b, _ := json.Marshal(map[string]string{
		"auto_blocklist_reason": reason,
		"auto_blocklist_time":   time.Now().UTC().Format(time.RFC3339),
	})
	return string(b)
}

// denmaCheckOptin wraps listmonk's opt-in sender: a subscriber whose domain
// can't receive mail is blocklisted and sent nothing, and the sign-up is
// told why. Called by buildApp.
func denmaCheckOptin(send func(models.Subscriber, []int) (int, error), db *sqlx.DB, ko *koanf.Koanf) func(models.Subscriber, []int) (int, error) {
	skip := denmaEmailSkip(ko)
	return func(sub models.Subscriber, listIDs []int) (int, error) {
		bad, reason := denmaEmailBad(sub.Email, skip)
		if !bad {
			return send(sub, listIDs)
		}
		lo.Printf("denma: blocklisting %s: %s", sub.Email, reason)
		denmaBlocklist(db, sub.Email, reason)
		return 0, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("%s can't receive e-mail: %s. Check the address.", sub.Email, reason))
	}
}

// denmaStartEmailChecks adds the job that checks the subscribers created or
// changed in the last day to the app's cron. Called by buildApp.
func denmaStartEmailChecks(a *App) {
	if a.crons == nil || (a.ko.Bool("denma.multi_center") && a.ko.String("denma.center") == "") {
		return // the hub has no subscribers
	}
	var (
		mu      sync.Mutex
		skip    = denmaEmailSkip(a.ko)
		checked = map[string]time.Time{} // "id email" -> when
	)
	if _, err := a.crons.Add("@every 1m", func() {
		if !mu.TryLock() {
			return
		}
		defer mu.Unlock()

		for k, at := range checked {
			if time.Since(at) > 25*time.Hour {
				delete(checked, k)
			}
		}

		var subs []struct {
			ID    int    `db:"id"`
			Email string `db:"email"`
		}
		if err := a.db.Select(&subs, `SELECT id, email FROM subscribers
			WHERE status <> 'blocklisted' AND updated_at > NOW() - INTERVAL '1 day' ORDER BY id`); err != nil {
			a.log.Printf("denma: error getting subscribers to check: %v", err)
			return
		}
		looked := 0
		for _, s := range subs {
			key := fmt.Sprintf("%d %s", s.ID, s.Email)
			if _, ok := checked[key]; ok {
				continue
			}
			i := strings.LastIndex(s.Email, "@")
			domain := strings.ToLower(s.Email[i+1:])
			if skip[domain] {
				checked[key] = time.Now()
				continue
			}

			denmaDomains.Lock()
			_, cached := denmaDomains.m[domain]
			denmaDomains.Unlock()
			if !cached {
				if looked >= denmaDomainsPerRun {
					continue // next minute
				}
				looked++
			}
			bad, reason, known := denmaCheckDomain(domain)
			if !known {
				continue // try again next minute
			}
			checked[key] = time.Now()
			if bad {
				a.log.Printf("denma: blocklisting %s: %s", s.Email, reason)
				denmaBlocklist(a.db, s.Email, reason)
			}
		}
	}); err != nil {
		a.log.Printf("denma: error starting e-mail checks: %v", err)
		return
	}
	a.crons.Start()
}
