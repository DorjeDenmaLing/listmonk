package main

// denma: deleting a Media file is checked against what shows it: scheduled,
// running and paused campaigns (their own content, their template's, or as an
// attachment), active automations (through their templates), templates, the
// center's design, and the logo and icon settings. If anything does, the
// delete is refused with the list until it's confirmed (?confirm=true); the
// Media page asks first (assets/js/denma-media.js). Found by the file's name
// in their links, so a file with the same name elsewhere also counts.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaMediaUsesShown is how many of each kind a list names.
const denmaMediaUsesShown = 8

// denmaFileLink matches any of a file's names (as written or in a URL) as the
// end of a link.
func denmaFileLink(names []string) *regexp.Regexp {
	var alts []string
	for _, n := range names {
		alts = append(alts, regexp.QuoteMeta(n), regexp.QuoteMeta(url.PathEscape(n)))
	}
	return regexp.MustCompile(`(?:^|/)(?:` + strings.Join(alts, "|") + `)(?:["'?#)\s\\<]|$)`)
}

// denmaUseList lists names, up to denmaMediaUsesShown, with how many more.
func denmaUseList(label string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := names
	if len(shown) > denmaMediaUsesShown {
		shown = shown[:denmaMediaUsesShown]
	}
	s := strings.Join(shown, ", ")
	if label != "" {
		s = label + " " + s
	}
	if more := len(names) - len(shown); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

type denmaUseRow struct {
	ID       int    `db:"id"`
	Name     string `db:"name"`
	Kind     string `db:"kind"`
	Attached bool   `db:"attached"`
	Text     string `db:"text"`
}

// denmaMediaUses lists, in words, what shows Media file id (none if it isn't
// there).
func (a *App) denmaMediaUses(id int) ([]string, error) {
	var m struct {
		Filename string `db:"filename"`
		Thumb    string `db:"thumb"`
	}
	if err := a.db.Get(&m, `SELECT filename, COALESCE(thumb, '') AS thumb FROM media WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, a.denmaMediaUsesErr(err)
	}
	names := []string{m.Filename}
	if m.Thumb != "" {
		names = append(names, m.Thumb)
	}
	re := denmaFileLink(names)
	var like []string // for the database to find them first
	for _, n := range names {
		like = append(like, n)
		if e := url.PathEscape(n); e != n {
			like = append(like, e)
		}
	}
	has := func(text string) string {
		return `EXISTS (SELECT 1 FROM unnest($1::TEXT[]) n WHERE strpos(` + text + `, n) > 0)`
	}

	// Campaigns that haven't finished.
	campText := `c.body || COALESCE(c.body_source, '') || COALESCE(c.altbody, '') || COALESCE(t.body, '') || COALESCE(t.body_source, '')`
	var camps []denmaUseRow
	if err := a.db.Select(&camps, `SELECT c.id, c.name, c.status::TEXT AS kind, `+campText+` AS text,
		EXISTS (SELECT 1 FROM campaign_media cm WHERE cm.campaign_id = c.id AND cm.media_id = $2) AS attached
		FROM campaigns c LEFT JOIN templates t ON t.id = c.template_id
		WHERE c.status IN ('scheduled', 'running', 'paused')
			AND (`+has(campText)+` OR EXISTS (SELECT 1 FROM campaign_media cm WHERE cm.campaign_id = c.id AND cm.media_id = $2))
		ORDER BY c.id`, pq.Array(like), id); err != nil {
		return nil, a.denmaMediaUsesErr(err)
	}
	var campNames []string
	for _, c := range camps {
		if c.Attached || re.MatchString(c.Text) {
			campNames = append(campNames, fmt.Sprintf("the %s campaign %q", c.Kind, c.Name))
		}
	}

	// Templates, and the center's design.
	tplText := `t.body || COALESCE(t.body_source, '')`
	var tpls []denmaUseRow
	if err := a.db.Select(&tpls, `SELECT t.id, t.name, t.type::TEXT AS kind, `+tplText+` AS text, FALSE AS attached
		FROM templates t WHERE `+has(tplText)+` ORDER BY t.id`, pq.Array(like)); err != nil {
		return nil, a.denmaMediaUsesErr(err)
	}
	var tplIDs []int64
	var tplNames []string
	design := false
	for _, t := range tpls {
		if !re.MatchString(t.Text) {
			continue
		}
		tplIDs = append(tplIDs, int64(t.ID))
		if t.Kind == denmaTemplateTypeDesign {
			design = true
		} else {
			tplNames = append(tplNames, fmt.Sprintf("%q", t.Name))
		}
	}

	// Active automations, whose e-mails are made from those templates.
	var autoNames []string
	if len(tplIDs) > 0 && denmaAutomationsOn(a) {
		var autos []denmaUseRow
		if err := a.db.Select(&autos, `SELECT a.id, a.name, '' AS kind, '' AS text, FALSE AS attached FROM denma_automations a
			WHERE a.active AND a.send_email AND (a.template_id = ANY($1) OR EXISTS (
				SELECT 1 FROM denma_automation_steps s WHERE s.automation_id = a.id AND s.template_id = ANY($1)))
			ORDER BY a.id`, pq.Array(tplIDs)); err != nil {
			return nil, a.denmaMediaUsesErr(err)
		}
		for _, x := range autos {
			autoNames = append(autoNames, fmt.Sprintf("%q", x.Name))
		}
	}

	// The settings that point at it.
	keys := []string{"app.logo_url", "app.favicon_url"}
	labels := map[string]string{"app.logo_url": "the logo setting", "app.favicon_url": "the favicon setting"}
	if denmaIsHubKo(a.ko) {
		keys = append(keys, "denma.admin_logo_url", "denma.admin_icon_url")
		labels["denma.admin_logo_url"], labels["denma.admin_icon_url"] = "the admin logo setting", "the admin icon setting"
	}
	var sets []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := a.db.Select(&sets, `SELECT key, COALESCE(value #>> '{}', '') AS value FROM settings WHERE key = ANY($1) ORDER BY key`, pq.Array(keys)); err != nil {
		return nil, a.denmaMediaUsesErr(err)
	}
	var setNames []string
	for _, s := range sets {
		if re.MatchString(s.Value) {
			setNames = append(setNames, labels[s.Key])
		}
	}

	var out []string
	for _, s := range []string{
		denmaUseList("", campNames),
		denmaUseList(plural(len(autoNames), "the active automation", "the active automations"), autoNames),
		denmaUseList(plural(len(tplNames), "the template", "the templates"), tplNames),
	} {
		if s != "" {
			out = append(out, s)
		}
	}
	if design {
		out = append(out, "the center's design")
	}
	out = append(out, setNames...)
	return slices.Compact(out), nil
}

func (a *App) denmaMediaUsesErr(err error) error {
	a.log.Printf("denma: error finding what uses a media file: %v", err)
	return echo.NewHTTPError(http.StatusInternalServerError, "Error checking what uses the file.")
}

// DenmaMediaUses answers what shows a Media file.
func (a *App) DenmaMediaUses(c echo.Context) error {
	uses, err := a.denmaMediaUses(getID(c))
	if err != nil {
		return err
	}
	if uses == nil {
		uses = []string{}
	}
	return c.JSON(http.StatusOK, okResp{map[string][]string{"uses": uses}})
}

// denmaMediaGuard is a media file's delete, refused with what shows the file
// unless confirmed (?confirm=true), and with the automations' remembered
// checks forgotten after it (cmd/denma_automations.go).
func (a *App) denmaMediaGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.QueryParam("confirm") != "true" {
			uses, err := a.denmaMediaUses(getID(c))
			if err != nil {
				return err
			}
			if len(uses) > 0 {
				return echo.NewHTTPError(http.StatusConflict, "This file is shown by "+strings.Join(uses, "; ")+
					". Deleting it leaves the image missing from them. To delete it anyway, confirm it (confirm=true).")
			}
		}
		err := next(c)
		if err == nil {
			a.denmaForgetAutoChecks()
		}
		return err
	}
}
