package utils

import "testing"

// SanitizeURI gives only a path from the root, and nothing a browser reads as another host.
func TestDenmaSanitizeURI(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/admin/lists?x=1":       "/admin/lists",
		"https://evil.com/a/b":   "/a/b",
		"//evil.com/x":           "/x",
		"/\\evil.com":            "/",
		"\\\\evil.com":           "/",
		"/%5Cevil.com":           "/",
		"/%09/evil.com":          "/",
		"/%0d%0aSet-Cookie:x":    "/",
		"/a/../b":                "/",
		"javascript:alert(1)":    "/",
		"admin":                  "/",
		"/%2Fevil.com":           "/evil.com",
		"/campaigns/new":         "/campaigns/new",
		"/c/halifax/admin/lists": "/c/halifax/admin/lists",
	} {
		if got := SanitizeURI(in); got != want {
			t.Errorf("SanitizeURI(%q) = %q, want %q", in, got, want)
		}
	}
}
