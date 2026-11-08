package main

// denma: limits on wrong passwords, two-factor codes and forgotten-password requests at the sign-in pages.

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
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

// denmaThrottleKeys is how many keys a throttle keeps before it drops the expired ones.
const denmaThrottleKeys = 10000

var (
	denmaPasswordByAccount = newDenmaThrottle(10, 15*time.Minute)
	denmaPasswordByIP      = newDenmaThrottle(30, 15*time.Minute)
	denmaPasswordByDevice  = newDenmaThrottle(10, 15*time.Minute)
	denmaPasswordByUser    = newDenmaThrottle(10, 15*time.Minute)
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

// denmaIPKey is the key a client address counts under: the address, or its /64 for IPv6.
func denmaIPKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// denmaClientKey is the key the request's client counts under.
func denmaClientKey(c echo.Context) string {
	return denmaIPKey(c.RealIP())
}

// denmaCurrentPassword checks a signed-in user's current password with the wrong-password limit.
func denmaCurrentPassword(key string, check func() (bool, error)) (bool, error) {
	if wait := denmaPasswordByUser.wait(key); wait > 0 {
		return false, denmaTooMany(wait)
	}
	right, err := check()
	switch {
	case err != nil:
		return false, err
	case right:
		denmaPasswordByUser.clear(key)
	default:
		denmaPasswordByUser.add(key)
	}
	return right, nil
}

// denmaAccountKey is the account a sign-in names, in any case.
func denmaAccountKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// denmaPersonKey is the account a /login sign-in counts under, and the person signing in (0 if none).
func (a *App) denmaPersonKey(name string) (string, int) {
	if id := a.denmaPersonNamed(name); id != 0 {
		return "person:" + strconv.Itoa(id), id
	}
	return denmaAccountKey(name), 0
}

// denmaPersonNamed is the person whose e-mail address name is, else whose username, or 0.
func (a *App) denmaPersonNamed(name string) int {
	var id int
	_ = a.db.Get(&id, `SELECT id FROM denma.people WHERE LOWER(username) = LOWER($1) OR LOWER(email) = LOWER($1)
		ORDER BY LOWER(email) = LOWER($1) DESC, id LIMIT 1`, strings.TrimSpace(name))
	return id
}

// denmaPasswordWait is the error for a password that may not be tried now, or nil.
func (a *App) denmaPasswordWait(c echo.Context, account string) error {
	wait := max(denmaPasswordByAccount.wait(account), denmaPasswordByIP.wait(denmaClientKey(c)))
	if dev := a.denmaDevice(c, account); dev != "" {
		wait = denmaPasswordByDevice.wait(dev)
	}
	if wait > 0 {
		return denmaTooMany(wait)
	}
	return nil
}

// denmaPasswordResult counts a wrong password, or clears the account's count for a right one.
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
	denmaPasswordByIP.add(denmaClientKey(c))
}

const (
	denmaDeviceCookie = "denma_device"
	denmaDeviceDays   = 180
)

// denmaDevice is the browser's token's hash if its cookie is for account, else "".
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

// denmaRememberDevice gives the browser a cookie for account, or renews its own (dev).
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

// denmaForgotAllowed reports whether a forgotten password e-mail may go to email now, counting it.
func denmaForgotAllowed(c echo.Context, email string) bool {
	ip := denmaClientKey(c)
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

// denmaTOTPUsed is the two-factor codes used, by account, for as long as a code is good.
var denmaTOTPUsed = struct {
	sync.Mutex
	m map[string]time.Time // account + code -> until
}{m: map[string]time.Time{}}

// denmaTOTPOnce reports whether code is account's (secret) and unused, and marks it used.
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

// denmaHubTwofa checks a hub user's two-factor code as people's are (DenmaLoginTwofa); each works once.
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
