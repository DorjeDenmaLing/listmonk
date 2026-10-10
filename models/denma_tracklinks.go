package models

// denma: every http(s) link in a campaign is tracked. CompileTemplate turns
// the links of its template, body and plain-text version into
// {{ TrackLink "..." . }}. Links made by template tags (unsubscribe, view in
// browser) and links with template tags in them are left as they are.

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	reDenmaAnchor = regexp.MustCompile(`(?is)<(?:a|area)\b[^>]*>`)
	reDenmaHref   = regexp.MustCompile(`(?is)(\bhref\s*=\s*)(?:"([^"]*)"|'([^']*)')`)
	reDenmaURL    = regexp.MustCompile(`(?i)https?://[^\s<>"'{}]+`)
	reDenmaAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
)

// DenmaTrackHTML tracks the href of each <a> and <area> tag.
func DenmaTrackHTML(s string) string {
	return reDenmaAnchor.ReplaceAllStringFunc(s, func(tag string) string {
		return reDenmaHref.ReplaceAllStringFunc(tag, func(attr string) string {
			m := reDenmaHref.FindStringSubmatch(attr)
			q, v := `"`, m[2]
			if strings.HasPrefix(strings.TrimSpace(attr[len(m[1]):]), "'") {
				q, v = `'`, m[3]
			}
			u := html.UnescapeString(strings.TrimSpace(v))
			if !denmaTrackable(u) {
				return attr
			}
			return m[1] + q + denmaTrackLink(u) + q
		})
	})
}

// DenmaTrackText tracks the URLs in plain text, outside template actions.
func DenmaTrackText(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range reDenmaAction.FindAllStringIndex(s, -1) {
		b.WriteString(denmaTrackURLs(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(denmaTrackURLs(s[last:]))
	return b.String()
}

// denmaTrackBody tracks a campaign body's links, by its content type.
func (c *Campaign) denmaTrackBody(body string) string {
	if c.ContentType == CampaignContentTypePlain {
		return DenmaTrackText(body)
	}
	return DenmaTrackHTML(body)
}

func denmaTrackURLs(s string) string {
	return reDenmaURL.ReplaceAllStringFunc(s, func(u string) string {
		// Punctuation after a URL isn't part of it; nor is a closing
		// bracket without an opening one.
		trail := ""
		for len(u) > 0 {
			last := u[len(u)-1]
			if strings.IndexByte(".,;:!?", last) >= 0 ||
				(last == ')' && strings.Count(u, "(") < strings.Count(u, ")")) ||
				(last == ']' && strings.Count(u, "[") < strings.Count(u, "]")) {
				trail = string(last) + trail
				u = u[:len(u)-1]
				continue
			}
			break
		}
		if !denmaTrackable(u) {
			return u + trail
		}
		return denmaTrackLink(u) + trail
	})
}

func denmaTrackable(u string) bool {
	l := strings.ToLower(u)
	return (strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")) && len(u) > len("https://") &&
		!strings.Contains(u, "{{") && !strings.HasSuffix(u, "@TrackLink")
}

func denmaTrackLink(u string) string {
	return `{{ TrackLink ` + strconv.Quote(u) + ` . }}`
}
