package main

// denma: limits on guessing at the sign-in pages, which anyone can reach
// (/login is outside Cloudflare Access). listmonk has none: a password could
// be tried without end, and each try is a bcrypt in Postgres, on the
// connections every center shares.
//
// Failures are counted in memory (one process serves every center), over a
// sliding window, by account and by client address (c.RealIP(), which is the
// visitor's only when the web server passes it on: listmonk-infra/
// Caddyfile.multi-center):
//   - passwords (/login, and the hub's /admin/login): 10 wrong for one
//     account (a person's username and e-mail address together), or 30 from
//     one address, in 15 minutes;
//   - two-factor codes: 5 wrong for one person in 15 minutes, whichever
//     sign-in they came in;
//   - forgotten passwords (/login/forgot): 3 e-mails to one address, and 10
//     requests from one client address, an hour.
// Past a limit, nothing is checked or sent until the oldest failure is out of
// the window; the page says when to try again. Counting unknown names too
// means the answer doesn't tell whether an account exists. A right password
// clears its account's count, not its address's.

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// denmaThrottle counts events by key within a sliding window.
type denmaThrottle struct {
	max    int
	window time.Duration

	mu   sync.Mutex
	hits map[string][]time.Time
}

func newDenmaThrottle(max int, window time.Duration) *denmaThrottle {
	return &denmaThrottle{max: max, window: window, hits: map[string][]time.Time{}}
}

// denmaThrottleKeys is how many keys a throttle keeps before it drops the
// expired ones, so made-up names can't grow it without end.
const denmaThrottleKeys = 10000

var (
	denmaPasswordByAccount = newDenmaThrottle(10, 15*time.Minute)
	denmaPasswordByIP      = newDenmaThrottle(30, 15*time.Minute)
	denmaTwofaByPerson     = newDenmaThrottle(5, 15*time.Minute)
	denmaForgotByEmail     = newDenmaThrottle(3, time.Hour)
	denmaForgotByIP        = newDenmaThrottle(10, time.Hour)
)

// recent is key's events still in the window (with t.mu held).
func (t *denmaThrottle) recent(key string, now time.Time) []time.Time {
	ts := t.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= t.window {
		i++
	}
	if i == len(ts) {
		delete(t.hits, key)
		return nil
	}
	ts = ts[i:]
	t.hits[key] = ts
	return ts
}

// wait is how long until key may try again: 0 while it's under the limit.
func (t *denmaThrottle) wait(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	ts := t.recent(key, now)
	if len(ts) < t.max {
		return 0
	}
	return ts[len(ts)-t.max].Add(t.window).Sub(now)
}

// add counts an event for key.
func (t *denmaThrottle) add(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if len(t.hits) >= denmaThrottleKeys {
		for k := range t.hits {
			t.recent(k, now)
		}
	}
	ts := append(t.recent(key, now), now)
	if len(ts) > t.max {
		ts = ts[len(ts)-t.max:] // only the last max matter
	}
	t.hits[key] = ts
}

// clear forgets key's events.
func (t *denmaThrottle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.hits, key)
}

// denmaTooMany is the error for a limit reached, saying when to try again.
func denmaTooMany(wait time.Duration) error {
	mins := int(math.Ceil(wait.Minutes()))
	when := "in a minute"
	if mins > 1 {
		when = fmt.Sprintf("in %d minutes", mins)
	}
	return echo.NewHTTPError(http.StatusTooManyRequests, "Too many attempts. Try again "+when+".")
}

// denmaAccountKey is the account a sign-in names, in any case.
func denmaAccountKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// denmaPersonKey is the account a sign-in at /login counts under: the person
// whose username or e-mail address it is, so that both count together; else
// the name.
func (a *App) denmaPersonKey(name string) string {
	var id int
	if err := a.db.Get(&id, `SELECT id FROM denma.people WHERE LOWER(username) = LOWER($1) OR LOWER(email) = LOWER($1) LIMIT 1`,
		strings.TrimSpace(name)); err == nil {
		return "person:" + strconv.Itoa(id)
	}
	return denmaAccountKey(name)
}

// denmaPasswordWait is the error for a password that may not be tried now
// (for this account, or from this address), or nil.
func denmaPasswordWait(c echo.Context, account string) error {
	wait := max(denmaPasswordByAccount.wait(account), denmaPasswordByIP.wait(c.RealIP()))
	if wait > 0 {
		return denmaTooMany(wait)
	}
	return nil
}

// denmaPasswordResult counts a wrong password, or clears the account's count
// for a right one.
func denmaPasswordResult(c echo.Context, account string, ok bool) {
	if ok {
		denmaPasswordByAccount.clear(account)
		return
	}
	denmaPasswordByAccount.add(account)
	denmaPasswordByIP.add(c.RealIP())
}

// denmaForgotAllowed reports whether a forgotten password e-mail may go to
// email now, counting it if so. Each request counts for the client's address.
func denmaForgotAllowed(c echo.Context, email string) bool {
	ip := c.RealIP()
	if denmaForgotByIP.wait(ip) > 0 {
		return false
	}
	denmaForgotByIP.add(ip)
	key := denmaAccountKey(email)
	if denmaForgotByEmail.wait(key) > 0 {
		return false
	}
	denmaForgotByEmail.add(key)
	return true
}
