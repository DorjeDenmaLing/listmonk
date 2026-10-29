package main

// denma: images missing from an e-mail. An image in a campaign or automation
// e-mail that's in the center's Media is shown from there; deleted from
// Media since, it would be a broken image in everyone's e-mail. So a
// campaign doesn't start, nor is scheduled or tested, and an automation isn't
// saved, turned on or run, while its e-mail (as rendered) has an image from the
// center's uploads that its Media no longer has. Images from elsewhere aren't
// checked. Deleting a file is checked against what shows it too
// (cmd/denma_mediause.go).
//
// Nor while it has one at the hub's uploads address that the hub's Media
// doesn't have: a center's files were there before centers had their own
// folder (DDL's, until 2026-10-23), and nothing serves them there now.
// Features version 42 pointed the centers' templates and campaigns at their
// own folder, for the files their Media has (denmaRewriteHubUploads).

import (
	"database/sql"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

var reDenmaImgSrc = regexp.MustCompile(`(?i)<img\b[^>]*?\ssrc\s*=\s*["']([^"']+)["']`)

// denmaUploadNames is the names of the files under an uploads address (base,
// absolute; its path counts too) that an e-mail's images (body, HTML) show.
func denmaUploadNames(base string, body []byte) []string {
	u, err := url.Parse(base)
	if err != nil || u.RawQuery != "" { // a private bucket's signed URLs: unknown
		return nil
	}
	var names []string
	for _, m := range reDenmaImgSrc.FindAllSubmatch(body, -1) {
		src := html.UnescapeString(string(m[1]))
		var rest string
		switch {
		case strings.HasPrefix(src, base):
			rest = src[len(base):]
		case u.Path != "" && u.Path != "/" && strings.HasPrefix(src, u.Path):
			rest = src[len(u.Path):]
		default:
			continue
		}
		if i := strings.IndexAny(rest, "?#"); i >= 0 {
			rest = rest[:i]
		}
		name, err := url.PathUnescape(rest)
		if err != nil || name == "" || strings.Contains(name, "/") || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// denmaMediaNames is the names of the files in the center's uploads that an
// e-mail's images (body, HTML) show.
func (a *App) denmaMediaNames(body []byte) []string {
	if a.media == nil {
		return nil
	}
	return denmaUploadNames(a.media.GetURL(""), body)
}

// denmaHubImages is the names of the images in a center's e-mail (body, HTML)
// at the hub's uploads address that the hub's Media doesn't have.
func (a *App) denmaHubImages(body []byte) []string {
	if denmaHub == nil || a.ko.String("denma.center") == "" {
		return nil
	}
	hub := denmaHub.current()
	if hub == nil || hub.media == nil {
		return nil
	}
	names := denmaUploadNames(hub.media.GetURL(""), body)
	if len(names) == 0 {
		return nil
	}
	return hub.denmaNotInMedia(names)
}

// denmaNotInMedia is the names that aren't files in the app's Media.
func (a *App) denmaNotInMedia(names []string) []string {
	var have []string
	if err := a.db.Select(&have, `SELECT n FROM unnest($1::TEXT[]) n
		WHERE EXISTS (SELECT 1 FROM media WHERE filename = n OR thumb = n)`, pq.Array(names)); err != nil {
		a.log.Printf("denma: error checking an e-mail's images: %v", err)
		return nil
	}
	var missing []string
	for _, n := range names {
		if !slices.Contains(have, n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// denmaCheckImages refuses an e-mail with images from the center's uploads
// that aren't in its Media, or at the hub's address.
func (a *App) denmaCheckImages(body []byte) error {
	if names := a.denmaMediaNames(body); len(names) > 0 {
		switch missing := a.denmaNotInMedia(names); len(missing) {
		case 0:
		case 1:
			return fmt.Errorf("The image %s isn't in the center's Media any more, so it would be missing from the e-mail. Upload it again and put it back in, or take it out.", missing[0])
		default:
			return fmt.Errorf("These images aren't in the center's Media any more, so they would be missing from the e-mail: %s. Upload them again and put them back in, or take them out.", orList(missing, "and"))
		}
	}
	switch old := a.denmaHubImages(body); len(old) {
	case 0:
	case 1:
		return fmt.Errorf("The image %s is linked at an old address (/uploads/%s), which no longer serves the center's files, so it would be missing from the e-mail. Put it in again from the center's Media.", old[0], old[0])
	default:
		return fmt.Errorf("These images are linked at an old address (/uploads/), which no longer serves the center's files, so they would be missing from the e-mail: %s. Put them in again from the center's Media.", orList(old, "and"))
	}
	return nil
}

// denmaHubUploadsRewriter is a function that points HTML's (or a visual
// source's) links to files named at the hub's uploads address (root, uri),
// absolute or root-relative, to base.
func denmaHubUploadsRewriter(root, uri, base string, names []string) (func(string) string, error) {
	alts := make([]string, len(names))
	for i, n := range names {
		alts[i] = regexp.QuoteMeta(n)
	}
	re, err := regexp.Compile(`(` + regexp.QuoteMeta(root) + `|["'(=\s])` + regexp.QuoteMeta(uri+"/") +
		`(` + strings.Join(alts, "|") + `)(["'?#)\s\\]|$)`)
	if err != nil {
		return nil, err
	}
	return func(s string) string {
		return re.ReplaceAllStringFunc(s, func(m string) string {
			g := re.FindStringSubmatch(m)
			if g[1] == root {
				g[1] = ""
			}
			return g[1] + base + g[2] + g[3]
		})
	}, nil
}

// denmaRewriteHubUploads points the links in the center's templates and
// campaigns (in tx, its schema) to its files at the hub's uploads address
// (root and uri: "https://news.example.org", "/uploads"), absolute or
// root-relative, to its own uploads address (base, ending in "/"), for the
// files its Media has and the hub's (hubNames, served at that address) hasn't.
// Returns how many templates and campaigns it changed.
func denmaRewriteHubUploads(tx *sqlx.Tx, root, uri, base string, hubNames []string) (int, error) {
	if hubNames == nil {
		hubNames = []string{} // not NULL, which <> ALL would make no names
	}
	var names []string
	if err := tx.Select(&names, `SELECT n FROM (SELECT filename AS n FROM media UNION SELECT thumb FROM media WHERE thumb <> '') x
		WHERE n <> ALL($1::TEXT[])`, pq.Array(hubNames)); err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return 0, nil
	}
	rewrite, err := denmaHubUploadsRewriter(root, uri, base, names)
	if err != nil {
		return 0, err
	}
	fix := func(s *string) bool {
		if s == nil {
			return false
		}
		out := rewrite(*s)
		changed := out != *s
		*s = out
		return changed
	}

	n := 0
	for _, t := range []struct{ table, cols, text string }{
		{"templates", "body, body_source, NULL::TEXT AS altbody", "body || COALESCE(body_source, '')"},
		{"campaigns", "body, body_source, altbody", "body || COALESCE(body_source, '') || COALESCE(altbody, '')"},
	} {
		var rows []struct {
			ID      int     `db:"id"`
			Body    string  `db:"body"`
			Source  *string `db:"body_source"`
			AltBody *string `db:"altbody"`
		}
		if err := tx.Select(&rows, `SELECT id, `+t.cols+` FROM `+t.table+`
			WHERE strpos(`+t.text+`, $1) > 0`, uri+"/"); err != nil && err != sql.ErrNoRows {
			return n, err
		}
		for _, r := range rows {
			b, s, alt := fix(&r.Body), fix(r.Source), fix(r.AltBody)
			if !b && !s && !alt {
				continue
			}
			q := `UPDATE templates SET body = $2, body_source = $3 WHERE id = $1`
			args := []any{r.ID, r.Body, r.Source}
			if t.table == "campaigns" {
				q, args = `UPDATE campaigns SET body = $2, body_source = $3, altbody = $4 WHERE id = $1`, append(args, r.AltBody)
			}
			if _, err := tx.Exec(q, args...); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
