package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

// A key limited to addresses: IPs, networks and host names.
func TestDenmaFromAllowed(t *testing.T) {
	old := denmaLookupHost
	defer func() { denmaLookupHost = old }()
	denmaLookupHost = func(host string) []net.IP {
		if host == "app.example.org" {
			return []net.IP{net.ParseIP("198.51.100.7")}
		}
		return nil
	}
	allowed := []string{"203.0.113.4", "192.0.2.0/24", "app.example.org", "2001:db8::/32"}
	for ip, want := range map[string]bool{
		"203.0.113.4": true, "203.0.113.5": false, "192.0.2.200": true, "198.51.100.7": true,
		"198.51.100.8": false, "2001:db8::1": true, "2001:db9::1": false, "not an ip": false,
	} {
		if got := denmaFromAllowed(ip, allowed); got != want {
			t.Errorf("%s: allowed %v, want %v", ip, got, want)
		}
	}
	if !denmaFromAllowed("203.0.113.9", nil) {
		t.Error("a key with no addresses should be usable from anywhere")
	}
	for s, ok := range map[string]bool{"203.0.113.4": true, "10.0.0.0/8": true, "app.example.org": true,
		"localhost": false, "http://x.org": false, "a b": false} {
		if err := denmaCheckAllowedFrom(s); (err == nil) != ok {
			t.Errorf("%q: error %v, want ok %v", s, err, ok)
		}
	}
}

// The pause comes at the hub's "At risk" bands, with enough mail and
// bounces or complaints to go by.
func TestDenmaAPIPauseReason(t *testing.T) {
	for _, c := range []struct {
		r     denmaAPIRates
		pause bool
	}{
		{denmaAPIRates{Sends: 100, Hard: 50}, false},        // too little mail to tell
		{denmaAPIRates{Sends: 1000, Hard: 49}, false},       // under 5%
		{denmaAPIRates{Sends: 1000, Hard: 50}, true},        // 5%
		{denmaAPIRates{Sends: 200, Hard: 9}, false},         // 4.5%, and under 10 bounces
		{denmaAPIRates{Sends: 1000, Complaints: 2}, false},  // 0.2%, but only 2
		{denmaAPIRates{Sends: 1000, Complaints: 3}, true},   // 0.3%
		{denmaAPIRates{Sends: 10000, Complaints: 9}, false}, // 0.09%
		{denmaAPIRates{Sends: 10000, Complaints: 10}, true}, // 0.1%
		{denmaAPIRates{Sends: 10000, Hard: 400, Complaints: 5}, false},
	} {
		if got := denmaAPIPauseReason(c.r); (got != "") != c.pause {
			t.Errorf("%+v: %q, want pause %v", c.r, got, c.pause)
		}
	}
}

// Webhooks are signed over "<t>.<body>", and wait longer after each failure.
func TestDenmaWebhook(t *testing.T) {
	body := []byte(`{"events":[]}`)
	sig := denmaWebhookSignature("whsec_x", 1700000000, body)
	m := hmac.New(sha256.New, []byte("whsec_x"))
	m.Write([]byte("1700000000." + string(body)))
	if want := "t=1700000000,v1=" + hex.EncodeToString(m.Sum(nil)); sig != want {
		t.Fatalf("signature %s, want %s", sig, want)
	}
	for n, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 5: 16 * time.Minute, 9: 256 * time.Minute, 10: 6 * time.Hour, 50: 6 * time.Hour} {
		if got := denmaWebhookWait(n); got != want {
			t.Errorf("wait after %d failures: %v, want %v", n, got, want)
		}
	}
	for ip, public := range map[string]bool{"8.8.8.8": true, "127.0.0.1": false, "10.1.2.3": false, "169.254.169.254": false,
		"192.168.1.1": false, "::1": false, "fd00::1": false, "2606:4700::1": true,
		"100.64.0.1": false, "100.127.255.254": false, "100.128.0.1": true, "192.0.0.8": false, "198.18.0.1": false,
		"198.19.255.1": false, "0.1.2.3": false, "64:ff9b::a00:1": false, "64:ff9b:1::1": false, "::ffff:10.0.0.1": false} {
		if got := denmaPublicIP(net.ParseIP(ip)); got != public {
			t.Errorf("%s: public %v, want %v", ip, got, public)
		}
	}
}

// A webhook to a private address is refused, without saying what it is.
func TestDenmaWebhookPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	a := &App{ko: koanf.New(".")}
	err := a.postWebhook(srv.URL+"/hook", "whsec_x", nil)
	if !errors.Is(err, errDenmaPrivateAddr) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("got %v, want %v alone", err, errDenmaPrivateAddr)
	}
	_ = a.ko.Set("denma.api_private_webhooks", true)
	if err := a.postWebhook(srv.URL+"/hook", "whsec_x", nil); err != nil {
		t.Fatalf("with private webhooks allowed: %v", err)
	}
}

// "to" is an address or a list of them.
func TestDenmaAPITo(t *testing.T) {
	var r denmaAPIMessageReq
	if err := json.Unmarshal([]byte(`{"to": "a@x.org"}`), &r); err != nil || len(r.To) != 1 || r.To[0] != "a@x.org" {
		t.Fatalf("one address: %v %v", r.To, err)
	}
	if err := json.Unmarshal([]byte(`{"to": ["a@x.org", "b@x.org"]}`), &r); err != nil || len(r.To) != 2 {
		t.Fatalf("a list: %v %v", r.To, err)
	}
	if err := json.Unmarshal([]byte(`{"to": 5}`), &r); err == nil || !strings.Contains(err.Error(), "list of addresses") {
		t.Fatalf("a number: %v", err)
	}
}

// The key is sent as "Bearer <key>", the scheme in any case, with its
// prefix: an API key's, or a Server webhook's secret's.
func TestDenmaBearer(t *testing.T) {
	for hdr, want := range map[string]string{
		"Bearer lmk_abc": "lmk_abc", "bearer lmk_abc": "lmk_abc", "BEARER  lmk_abc ": "lmk_abc", " Bearer lmk_abc": "lmk_abc",
		"Bearer abc": "", "Basic lmk_abc": "", "lmk_abc": "", "Bearer": "", "": "", "Bearer lms_abc": "",
	} {
		key, ok := denmaBearer(hdr, denmaAPIKeyPrefix)
		if ok != (want != "") || (ok && key != want) {
			t.Errorf("%q: %q %v, want %q", hdr, key, ok, want)
		}
	}
	if key, ok := denmaBearer("Bearer lms_abc", denmaSignupSecretPrefix); !ok || key != "lms_abc" {
		t.Errorf("a webhook's secret: %q %v", key, ok)
	}
	if _, ok := denmaBearer("Bearer lmk_abc", denmaSignupSecretPrefix); ok {
		t.Error("an API key taken as a webhook's secret")
	}
}

// Keys are stored as their SHA-256, and look like keys.
func TestDenmaNewAPIKey(t *testing.T) {
	key, hash, hint, err := denmaNewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, denmaAPIKeyPrefix) || len(key) != len(denmaAPIKeyPrefix)+40 || hint != key[len(key)-4:] {
		t.Fatalf("key %q, hint %q", key, hint)
	}
	if hash != denmaAPIHash(key) || len(hash) != 64 {
		t.Fatalf("hash %q", hash)
	}
}

// A subscriber's attributes are added to, never replaced: new keys added,
// objects merged, arrays added to, and values they have kept.
func TestDenmaAppendJSON(t *testing.T) {
	dec := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	old := dec(`{"consent_at": "2026-01-01", "rg": {"person_id": 7, "registration_ids": [1]}, "consents": [{"at": "2026-01-01"}], "tags": ["a"]}`)
	add := dec(`{"consent_at": "2026-05-05", "rg": {"person_id": 7, "registration_ids": [1, 2]}, "consents": [{"at": "2026-01-01"}, {"at": "2026-05-05"}], "phone": "555", "x": null}`)
	got, _ := json.Marshal(denmaAppendJSON(old, add))
	want := `{"consent_at":"2026-01-01","consents":[{"at":"2026-01-01"},{"at":"2026-05-05"}],"phone":"555","rg":{"person_id":7,"registration_ids":[1,2]},"tags":["a"]}`
	if string(got) != want {
		t.Errorf("merged:\n%s\nwant\n%s", got, want)
	}

	// Another type kept; nothing to add from nothing.
	got, _ = json.Marshal(denmaAppendJSON(dec(`{"a": {"b": "s"}}`), dec(`{"a": {"b": {"c": 1}}}`)))
	if string(got) != `{"a":{"b":"s"}}` {
		t.Errorf("another type: %s", got)
	}
	got, _ = json.Marshal(denmaAppendJSON(dec(`{"a": 1}`), map[string]any(nil)))
	if string(got) != `{"a":1}` {
		t.Errorf("nothing added: %s", got)
	}
}

// A name given replaces only a stand-in.
func TestDenmaGivenName(t *testing.T) {
	for _, c := range []struct{ cur, given, want string }{
		{"ann", "Ann Lee", "Ann Lee"},         // the part before the @
		{"Ann@X.org", "Ann Lee", "Ann Lee"},   // the address
		{"", "Ann Lee", "Ann Lee"},            // none
		{"Annie Lee", "Ann Lee", "Annie Lee"}, // their own
		{"ann", "", "ann"},                    // none given
	} {
		if got := denmaGivenName(c.cur, c.given, "ann@x.org"); got != c.want {
			t.Errorf("%q, %q: %q, want %q", c.cur, c.given, got, c.want)
		}
	}
}

// A key does one or more of the API's things; subscribing isn't one any more.
func TestDenmaAPICanForm(t *testing.T) {
	if got, err := denmaAPICanForm([]string{"events", "messages", "messages"}); err != nil || strings.Join(got, ",") != "messages,events" {
		t.Errorf("tidied: %v %v", got, err)
	}
	if got, err := denmaAPICanForm([]string{"campaigns"}); err != nil || strings.Join(got, ",") != "campaigns" {
		t.Errorf("campaigns: %v %v", got, err)
	}
	if !denmaAPISends([]string{"messages"}) || !denmaAPISends([]string{"events", "campaigns"}) || denmaAPISends([]string{"events"}) {
		t.Error("which keys send")
	}
	for _, c := range []struct {
		can  []string
		want string
	}{
		{nil, "Choose what"},
		{[]string{"subscribers"}, "sign-up webhooks"},
		{[]string{"campaigns", "admin"}, `"admin" isn't`},
	} {
		if _, err := denmaAPICanForm(c.can); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.can, err, c.want)
		}
	}
}

// The OpenAPI description has every route, and no other, each with what a
// key needs for it (x-denma-can).
func TestDenmaAPIOpenAPI(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(denmaAPIOpenAPIDoc, &doc); err != nil {
		t.Fatal(err)
	}
	type op struct {
		Can string `json:"x-denma-can"`
	}
	described := map[string]bool{}
	for path, item := range doc.Paths {
		for method := range item {
			if method == "parameters" || path == "/openapi.json" {
				continue
			}
			described[strings.ToUpper(method)+" "+path] = true
		}
	}
	for _, r := range denmaAPIRoutes {
		path := strings.ReplaceAll(r.Path, ":id", "{id}")
		item, ok := doc.Paths[path][strings.ToLower(r.Method)]
		if !ok {
			t.Errorf("%s %s isn't described", r.Method, path)
			continue
		}
		delete(described, r.Method+" "+path)
		var o op
		if err := json.Unmarshal(item, &o); err != nil {
			t.Fatal(err)
		}
		if o.Can != r.Can {
			t.Errorf("%s %s: described as needing %q, needs %q", r.Method, path, o.Can, r.Can)
		}
	}
	for k := range described {
		t.Errorf("%s is described but isn't a route", k)
	}
}

// Replacing a key's value keeps the object's keys in order.
func TestDenmaJSONSet(t *testing.T) {
	got, err := denmaJSONSet([]byte(`{"b": 1, "servers": [{"url": "x"}], "a": {"c": [1, 2]}}`), "servers", []map[string]string{{"url": "y"}})
	if err != nil || string(got) != `{"b":1,"servers":[{"url":"y"}],"a":{"c": [1, 2]}}` {
		t.Errorf("%s %v", got, err)
	}
	if _, err := denmaJSONSet([]byte(`[1]`), "a", 1); err == nil {
		t.Error("an array taken as an object")
	}
}

// "confirmed" needs a consent record: when (the latest, to compare with an
// opt-out) and how; the first is the subscriber's consent_at and
// consent_source if they have none.
func TestDenmaConsentAt(t *testing.T) {
	dec := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	rec := dec(`{"consents": [{"at": "2026-01-01T00:00:00Z", "source": "form"}, {"at": "2026-05-05T10:00:00+02:00", "source": "registration form", "registration_id": 7}]}`)
	at, err := denmaConsentAt(rec)
	if err != nil || !at.Equal(time.Date(2026, 5, 5, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("the latest: %v %v", at, err)
	}
	if first, src := denmaFirstConsent(rec); src != "form" || !first.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the first: %v %q", first, src)
	}
	if _, src := denmaFirstConsent(dec(`{}`)); src != "" {
		t.Errorf("no consent's source: %q", src)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for name, attribs := range map[string]string{
		"none":       `{}`,
		"consent_at": `{"consent_at": "2026-01-01T00:00:00Z", "consent_source": "form"}`,
		"empty":      `{"consents": []}`,
		"no source":  `{"consents": [{"at": "2026-01-01T00:00:00Z"}]}`,
		"no time":    `{"consents": [{"at": "yesterday", "source": "form"}]}`,
		"future":     `{"consents": [{"at": "` + future + `", "source": "form"}]}`,
		"not a list": `{"consents": {"at": "2026-01-01T00:00:00Z", "source": "form"}}`,
	} {
		if _, err := denmaConsentAt(dec(attribs)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// No request is over denmaMaxBody, whether it says so or not.
func TestDenmaBodyLimit(t *testing.T) {
	e := echo.New()
	h := denmaBodyLimit(func(c echo.Context) error {
		_, err := io.Copy(io.Discard, c.Request().Body)
		return err
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.ContentLength = denmaMaxBody + 1
	if err := h(e.NewContext(req, httptest.NewRecorder())); err == nil || err.(*echo.HTTPError).Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a large Content-Length: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/", io.LimitReader(zeros{}, denmaMaxBody+10))
	req.ContentLength = -1
	if err := h(e.NewContext(req, httptest.NewRecorder())); err == nil {
		t.Error("a large body without a length was read")
	}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a": 1}`))
	if err := h(e.NewContext(req, httptest.NewRecorder())); err != nil {
		t.Errorf("a small body: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '0'
	}
	return len(p), nil
}

// Requests refused before anyone's known aren't in the activity log; failed
// sign-ins are.
func TestDenmaAuditUnauthed(t *testing.T) {
	e := echo.New()
	ctx := func(user, key bool) echo.Context {
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
		if user {
			c.Set(auth.UserHTTPCtxKey, auth.User{Base: auth.Base{ID: 3}})
		}
		if key {
			c.Set(denmaAPIKeyCtx, denmaAPIAuthed{ID: 2})
			c.Set(denmaSignupHookCtx, denmaSignupHook{ID: 4})
		}
		return c
	}
	for _, c := range []struct {
		route     string
		status    int
		user, key bool
		skip      bool
	}{
		{"/v1/messages", 401, false, false, true},
		{"/v1/messages", 200, false, false, true}, // can't be, but no key, no record
		{"/v1/messages", 400, false, true, false},
		{"/api/subscribers", 403, false, false, true},
		{"/api/subscribers", 401, false, false, true},
		{"/api/subscribers", 403, true, false, false},
		{"/signup/server", 401, false, false, true},
		{"/signup/server", 200, false, true, false},
		{"/login", 200, false, false, false},
		{"/admin/login", 401, false, false, false},
	} {
		if got := denmaAuditUnauthed(ctx(c.user, c.key), c.route, c.status); got != c.skip {
			t.Errorf("%s %d user %v key %v: skipped %v, want %v", c.route, c.status, c.user, c.key, got, c.skip)
		}
	}
}

// Links are found however they're written: entities, backslashes, no
// slashes, any attribute, CSS url() and its escapes; and checked again once
// a template's logic has made them.
func TestDenmaLinkHosts(t *testing.T) {
	for in, want := range map[string]string{
		`<a href="https://evil.example/x">`:                      "evil.example",
		`<a href="https:&#x2F;&#x2F;evil.example">`:              "evil.example",
		`<a href="https:&amp;#x2F;&amp;#x2F;evil.example">`:      "evil.example",
		`<a href="https:evil.example">`:                          "evil.example",
		`<a href="https:\\evil.example">`:                        "evil.example",
		`<td background="//evil.example/bg.png">`:                "evil.example",
		`<div style="background:url(//evil.example/a)">`:         "evil.example",
		`<div style="background:url('\68ttps://evil.example')">`: "evil.example",
		`<img src = " //evil.example/i.png">`:                    "evil.example",
		`see https://user@Evil.Example:8080/x`:                   "evil.example:8080",
		`<p>Use https if you can.</p>`:                           "",
		`<a href="/relative">x</a>`:                              "",
		"<a href=\"ht\ntps://evil.example\">":                    "evil.example",
		"<a href=\"https:/\t/evil.example\">":                    "evil.example",
		"<a href=\"https://good.example\n.evil.example/\">":      "good.example.evil.example",
		"<p>See https://good.example\nThanks</p>":                "good.example",
	} {
		if got := strings.Join(denmaTextLinkHosts(in), ","); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	// A value that's one address loses its tabs and newlines in an href.
	for in, want := range map[string]string{
		"ht\ntps://evil.example":              "evil.example",
		"https://good.example\n.evil.example": "good.example,good.example.evil.example",
		"Order 12\nhttps://good.example/x":   "good.example",
	} {
		if got := strings.Join(denmaLinkHosts(in), ","); got != want {
			t.Errorf("value %q: %q, want %q", in, got, want)
		}
	}
	for name, bad := range map[string]bool{"Ana Silva": false, "Dr. J.R. Smith": false, "Visit www.evil.example": true, "https://evil.example": true} {
		if denmaNameHasLink(name) != bad {
			t.Errorf("name %q: link %v, want %v", name, !bad, bad)
		}
	}
	allowed := []string{"center.org"}
	if err := denmaCheckRendered(allowed, `<img src="https://cdn.example/logo.png">`, `<img src="https://cdn.example/logo.png"><a href="https://www.center.org">`); err != nil {
		t.Errorf("the template's own link and the center's: %v", err)
	}
	if err := denmaCheckRendered(allowed, "", `<a href="https:/`+`/evil.example">`); err == nil {
		t.Error("a link a template made was allowed")
	}
}
