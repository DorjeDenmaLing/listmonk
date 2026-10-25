package main

// denma: images missing from an e-mail. An image in a campaign or automation
// e-mail that's in the center's Media is shown from there; deleted from
// Media since, it would be a broken image in everyone's e-mail. So a
// campaign doesn't start, nor is scheduled or tested, and an automation isn't
// saved or turned on, while its e-mail (as rendered) has an image from the
// center's uploads that its Media no longer has. Images from elsewhere aren't
// checked.

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/lib/pq"
)

var reDenmaImgSrc = regexp.MustCompile(`(?i)<img\b[^>]*?\ssrc\s*=\s*["']([^"']+)["']`)

// denmaMediaNames is the names of the files in the center's uploads that an
// e-mail's images (body, HTML) show.
func (a *App) denmaMediaNames(body []byte) []string {
	if a.media == nil {
		return nil
	}
	base := a.media.GetURL("")
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

// denmaCheckImages refuses an e-mail with images from the center's uploads
// that aren't in its Media.
func (a *App) denmaCheckImages(body []byte) error {
	names := a.denmaMediaNames(body)
	if len(names) == 0 {
		return nil
	}
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
	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("The image %s isn't in the center's Media any more, so it would be missing from the e-mail. Upload it again and put it back in, or take it out.", missing[0])
	}
	return fmt.Errorf("These images aren't in the center's Media any more, so they would be missing from the e-mail: %s. Upload them again and put them back in, or take them out.", orList(missing, "and"))
}
