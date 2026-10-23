package captcha

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type denmaRoundTrip func(*http.Request) (*http.Response, error)

func (f denmaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// hCaptcha's answer decides: one that isn't JSON fails, as a "no" does
// (biofool/listmonk#5: it passed).
func TestDenmaHCaptchaFailsClosed(t *testing.T) {
	for body, want := range map[string]bool{
		`<html>bad gateway</html>`: false,
		`{"success": false}`:       false,
		`{"success": true}`:        true,
	} {
		c := &Captcha{provider: ProviderHCaptcha, client: &http.Client{Transport: denmaRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}}
		if _, ok := c.verifyHCaptcha("token"); ok != want {
			t.Errorf("answer %s: ok %v, want %v", body, ok, want)
		}
	}
}
