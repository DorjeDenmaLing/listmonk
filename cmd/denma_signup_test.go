package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// Attributes the center keeps itself can't be given, but a Server webhook's
// consents.
func TestDenmaReservedAttrib(t *testing.T) {
	for _, c := range []struct {
		attribs  map[string]any
		consents bool
		want     string
	}{
		{map[string]any{"city": "Halifax", "member": map[string]any{"consent_at": "x"}}, false, ""}, // nested keys are theirs
		{map[string]any{"tags": []any{"a"}}, false, "tags"},
		{map[string]any{"consent_at": "2026-01-01T00:00:00Z"}, true, "consent_at"},
		{map[string]any{"consents": []any{}}, false, "consents"},
		{map[string]any{"consents": []any{}}, true, ""},
		{map[string]any{"auto_blocklist_reason": "x"}, true, "auto_blocklist_reason"},
		{map[string]any{"denma_api": 1}, false, "denma_api"},
		{map[string]any{"imported_at": "x"}, false, "imported_at"},
		{map[string]any{"zz": 1, "resubscribed_at": "x", "consent_source": "y"}, false, "consent_source"}, // the first by name
	} {
		if got := denmaReservedAttrib(c.attribs, c.consents); got != c.want {
			t.Errorf("%v (consents %v): %q, want %q", c.attribs, c.consents, got, c.want)
		}
	}
	if !strings.Contains(denmaReservedAttribMsg("consent_at"), "keeps itself") {
		t.Error("the message")
	}
}

// A Form webhook takes its fields, attributes and tags as JSON, or as a form,
// attributes as an object or each its own field.
func TestDenmaSignupRequest(t *testing.T) {
	e := echo.New()
	read := func(ct, body string) denmaSignupRequest {
		req := httptest.NewRequest(http.MethodPost, "/signup/x", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, ct)
		r, err := signupRequest(e.NewContext(req, httptest.NewRecorder()))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return r
	}

	r := read("application/json", `{"email": "a@x.org", "attribs": {"city": "Halifax", "n": 2}, "attribs.lang": "fr", "attribs[size]": "M",
		"tags": ["Retreat", "news"], "interests": ["a", "b"], "consent": true}`)
	if r.Fields["email"] != "a@x.org" || r.Fields["consent"] != "true" || r.Fields["interests"] != "a, b" {
		t.Errorf("fields: %v", r.Fields)
	}
	want := map[string]any{"city": "Halifax", "n": float64(2), "lang": "fr", "size": "M"}
	if !reflect.DeepEqual(r.Attribs, want) {
		t.Errorf("attributes: %v, want %v", r.Attribs, want)
	}
	if strings.Join(r.Tags, ",") != "news,retreat" {
		t.Errorf("tags: %v", r.Tags)
	}

	form := url.Values{"email": {"b@x.org"}, "attribs[city]": {"Boulder"}, "tags": {"a, b", "c"}, "name": {"Bea"}}
	r = read(echo.MIMEApplicationForm, form.Encode())
	if r.Fields["email"] != "b@x.org" || r.Fields["name"] != "Bea" || r.Attribs["city"] != "Boulder" || strings.Join(r.Tags, ",") != "a,b,c" {
		t.Errorf("a form: %v %v %v", r.Fields, r.Attribs, r.Tags)
	}
	if _, ok := r.Fields["attribs[city]"]; ok {
		t.Error("an attribute's field is also a field")
	}
}

// A Form webhook's token goes in its address; a Server webhook's secret is
// prefixed, for secret scanners. Both are stored as their SHA-256.
func TestDenmaNewSignupToken(t *testing.T) {
	tok, hash, hint, err := denmaNewSignupToken(denmaSignupForm)
	if err != nil || len(tok) != 32 || strings.HasPrefix(tok, denmaSignupSecretPrefix) || hash != denmaAPIHash(tok) || hint != tok[28:] {
		t.Errorf("form: %q %q %q %v", tok, hash, hint, err)
	}
	tok, hash, hint, err = denmaNewSignupToken(denmaSignupServer)
	if err != nil || len(tok) != len(denmaSignupSecretPrefix)+40 || !strings.HasPrefix(tok, denmaSignupSecretPrefix) ||
		hash != denmaAPIHash(tok) || hint != tok[len(tok)-4:] {
		t.Errorf("server: %q %q %q %v", tok, hash, hint, err)
	}
}

// The webhooks' OpenAPI description has their two addresses, each with the
// fields it reads.
func TestDenmaSignupOpenAPI(t *testing.T) {
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(denmaSignupOpenAPIDoc, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/{token}", "/server"} {
		if _, ok := doc.Paths[p]["post"]; !ok {
			t.Errorf("POST %s isn't described", p)
		}
	}
	if len(doc.Paths) != 2 {
		t.Errorf("%d paths described, want 2", len(doc.Paths))
	}
	keys := func(m map[string]json.RawMessage) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	// The Server webhook's request, as it's read (unknown fields refused).
	var server []string
	rt := reflect.TypeOf(denmaSignupServerReq{})
	for i := 0; i < rt.NumField(); i++ {
		server = append(server, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(server)
	if got := keys(doc.Components.Schemas["ServerSignup"].Properties); !slices.Equal(got, server) {
		t.Errorf("ServerSignup describes %v, the webhook reads %v", got, server)
	}
	form := append([]string{}, denmaSignupFormFields...)
	sort.Strings(form)
	if got := keys(doc.Components.Schemas["FormSignup"].Properties); !slices.Equal(got, form) {
		t.Errorf("FormSignup describes %v, the webhook reads %v", got, form)
	}
}

// A key's links can go only to its link domains and the center's, subdomains
// included, wherever they are in what it sends.
func TestDenmaLinks(t *testing.T) {
	hosts := denmaLinkHosts(
		"Reset it at https://Shop.Example.org/reset?t=1 or http://evil.test:8080/x.",
		map[string]any{"a": []any{"<a href=\"//cdn.evil.test/i.png\">", 5}, "b": "mailto:x@y.org"},
		[]map[string]any{{"url": "https://user@other.test/"}},
		`{{ TrackLink "https://news.example.org/a" }} <img src='https://img.example.org/p.png'>`,
	)
	want := []string{"Shop.Example.org", "evil.test:8080", "cdn.evil.test", "other.test", "news.example.org", "img.example.org"}
	sort.Strings(hosts)
	sort.Strings(want)
	if !slices.Equal(hosts, want) {
		t.Errorf("hosts %v, want %v", hosts, want)
	}
	allowed := []string{"example.org", "news.dorjedenmaling.org"}
	for h, ok := range map[string]bool{"example.org": true, "Shop.Example.org": true, "img.example.org.": true, "example.org:443": true,
		"badexample.org": false, "example.org.evil.test": false, "news.dorjedenmaling.org": true, "dorjedenmaling.org": false} {
		if got := denmaLinkAllowed(h, allowed); got != ok {
			t.Errorf("%s: allowed %v, want %v", h, got, ok)
		}
	}
	got, err := denmaLinkDomainsForm([]string{" Example.org ", "https://shop.example.org/x", "*.cdn.test", "example.org", ""})
	if err != nil || strings.Join(got, ",") != "example.org,shop.example.org,cdn.test" {
		t.Errorf("link domains: %v %v", got, err)
	}
	if _, err := denmaLinkDomainsForm([]string{"not a domain"}); err == nil {
		t.Error("a bad domain was taken")
	}
}

// Requests about people are recorded without their addresses, names or
// attributes.
func TestDenmaRedactPersonal(t *testing.T) {
	for route, personal := range map[string]bool{"/api/subscribers/:id": true, "/api/subscribers": true, "/api/import/subscribers": true,
		"/api/denma/people/:id": true, "/api/lists/:id": false, "/api/campaigns": false, "/api/denma/subscribers/resubscribe": true} {
		if got := denmaAuditPersonal(route); got != personal {
			t.Errorf("%s: personal %v, want %v", route, got, personal)
		}
	}
	in := map[string]any{"email": "a@x.org", "name": "Ann", "attribs": map[string]any{"city": "x"}, "lists": []any{float64(3)},
		"query": "subscribers.email = 'b@y.org'", "status": "enabled", "emails": []any{"c@z.org"}}
	out := denmaRedactPersonal(in).(map[string]any)
	for _, k := range []string{"email", "name", "attribs", "emails"} {
		if out[k] != "(not recorded)" {
			t.Errorf("%s: %v", k, out[k])
		}
	}
	if out["query"] != "subscribers.email = '(an address)'" || out["status"] != "enabled" || !reflect.DeepEqual(out["lists"], []any{float64(3)}) {
		t.Errorf("the rest: %v", out)
	}
	if b := denmaRedactPersonalJSON([]byte(`{"email": "a@x.org", "list_ids": [1]}`)); string(b) != `{"email":"(not recorded)","list_ids":[1]}` {
		t.Errorf("JSON: %s", b)
	}
	q := denmaRedactPersonal(map[string][]string{"query": {"a@x.org"}, "name": {"Ann"}, "page": {"2"}}).(map[string][]string)
	if q["query"][0] != "(an address)" || q["name"][0] != "(not recorded)" || q["page"][0] != "2" {
		t.Errorf("a query: %v", q)
	}
}

// A key reads its campaigns' events if it sends campaigns, the addresses'
// if it reads events.
func TestDenmaAPIKeyEventsArgs(t *testing.T) {
	args := denmaAPIKeyEventsArgs(5, 100, 7, []string{"campaigns"})
	if !reflect.DeepEqual(args, []any{int64(5), 100, 7, false, true}) {
		t.Errorf("campaigns: %v", args)
	}
	args = denmaAPIKeyEventsArgs(0, 10, 2, []string{"events", "messages"})
	if !reflect.DeepEqual(args, []any{int64(0), 10, 2, true, false}) {
		t.Errorf("events: %v", args)
	}
}
