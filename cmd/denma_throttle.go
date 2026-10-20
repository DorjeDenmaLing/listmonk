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
//
// So that guessing can't lock someone out (OWASP's device cookies): a
// browser whose password was right for an account is given a cookie for it
// (denmaDeviceCookie: a random token, kept hashed in denma.login_devices,
// for denmaDeviceDays after its last use). Its tries for that account count
// against its own limit of 10 wrong in 15 minutes, not the account's or its
// address's; the account's and the address's limits are for the browsers
// without one, so they shut out the guesser, not the account's own.

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/tmptokens"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/pquerna/otp/totp"
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
	denmaPasswordByDevice  = newDenmaThrottle(10, 15*time.Minute)
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
// (for this account, or from this address; or from this browser, one the
// account has signed in from), or nil.
func (a *App) denmaPasswordWait(c echo.Context, account string) error {
	wait := max(denmaPasswordByAccount.wait(account), denmaPasswordByIP.wait(c.RealIP()))
	if dev := a.denmaDevice(c, account); dev != "" {
		wait = denmaPasswordByDevice.wait(dev)
	}
	if wait > 0 {
		return denmaTooMany(wait)
	}
	return nil
}

// denmaPasswordResult counts a wrong password, or clears the account's count
// for a right one and remembers the browser for it.
func (a *App) denmaPasswordResult(c echo.Context, account string, ok bool) {
	dev := a.denmaDevice(c, account)
	if ok {
		denmaPasswordByAccount.clear(account)
		if dev != "" {
			denmaPasswordByDevice.clear(dev)
		}
		a.denmaRememberDevice(c, account, dev)
		return
	}
	if dev != "" {
		denmaPasswordByDevice.add(dev)
		return
	}
	denmaPasswordByAccount.add(account)
	denmaPasswordByIP.add(c.RealIP())
}

const (
	denmaDeviceCookie = "denma_device"
	denmaDeviceDays   = 180
)

// denmaDevice is the browser's token's hash if its cookie is for account,
// else "".
func (a *App) denmaDevice(c echo.Context, account string) string {
	ck, err := c.Cookie(denmaDeviceCookie)
	if err != nil || ck.Value == "" {
		return ""
	}
	hash := denmaAPIHash(ck.Value)
	var ok bool
	if err := a.db.Get(&ok, `SELECT EXISTS (SELECT 1 FROM denma.login_devices WHERE token_hash = $1 AND account = $2
		AND used_at > NOW() - make_interval(days => $3))`, hash, account, denmaDeviceDays); err != nil || !ok {
		return ""
	}
	return hash
}

// denmaRememberDevice gives the browser a cookie for account, or renews the
// one it has (dev, its hash).
func (a *App) denmaRememberDevice(c echo.Context, account, dev string) {
	token := ""
	if dev != "" {
		if _, err := a.db.Exec(`UPDATE denma.login_devices SET used_at = NOW() WHERE token_hash = $1`, dev); err != nil {
			a.log.Printf("denma: error renewing a sign-in's browser: %v", err)
		}
		ck, _ := c.Cookie(denmaDeviceCookie)
		token = ck.Value
	} else {
		var err error
		if token, err = utils.GenerateRandomString(32); err != nil {
			return
		}
		if _, err := a.db.Exec(`DELETE FROM denma.login_devices WHERE used_at < NOW() - make_interval(days => $1)`, denmaDeviceDays); err != nil {
			a.log.Printf("denma: error forgetting old sign-ins' browsers: %v", err)
		}
		if _, err := a.db.Exec(`INSERT INTO denma.login_devices (token_hash, account) VALUES ($1, $2)`, denmaAPIHash(token), account); err != nil {
			a.log.Printf("denma: error remembering a sign-in's browser: %v", err)
			return
		}
	}
	c.SetCookie(&http.Cookie{
		Name:     denmaDeviceCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Scheme() == "https" || c.Request().Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   denmaDeviceDays * 24 * 3600,
	})
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

// denmaTOTPUsed is the two-factor codes used, by account, for as long as a
// code is good (with the one step either side totp.Validate allows).
var denmaTOTPUsed = struct {
	sync.Mutex
	m map[string]time.Time // account + code -> until
}{m: map[string]time.Time{}}

// denmaTOTPOnce reports whether code is account's (secret) and hasn't been
// used yet, and marks it used: one seen over someone's shoulder, or
// replayed, doesn't sign anyone in again.
func denmaTOTPOnce(account, code, secret string) bool {
	if !totp.Validate(code, secret) {
		return false
	}
	now := time.Now()
	u := &denmaTOTPUsed
	u.Lock()
	defer u.Unlock()
	for k, until := range u.m {
		if now.After(until) {
			delete(u.m, k)
		}
	}
	k := account + "/" + code
	if _, used := u.m[k]; used {
		return false
	}
	u.m[k] = now.Add(3 * time.Minute)
	return true
}

// denmaHubTwofa checks a hub user's (a superadmin's) two-factor code, as
// people's are (DenmaLoginTwofa): 5 wrong ones in 15 minutes end the sign-in
// (its token), and each code works once. It returns the page's error if not.
func (a *App) denmaHubTwofa(token string, user auth.User, code string) (bool, string) {
	key := "hub:" + strconv.Itoa(user.ID)
	tooMany := func(wait time.Duration) (bool, string) {
		tmptokens.Delete(a.tmpKey(token))
		var he *echo.HTTPError
		errors.As(denmaTooMany(wait), &he)
		return false, fmt.Sprint(he.Message) + " Sign in again."
	}
	if wait := denmaTwofaByPerson.wait(key); wait > 0 {
		return tooMany(wait)
	}
	if !denmaTOTPOnce(key, code, user.TwofaKey.String) {
		denmaTwofaByPerson.add(key)
		if wait := denmaTwofaByPerson.wait(key); wait > 0 {
			return tooMany(wait)
		}
		return false, a.i18n.T("globals.messages.invalidValue")
	}
	denmaTwofaByPerson.clear(key)
	return true, ""
}
