package main

// denma: subscriber search. listmonk's advanced search takes a raw SQL
// expression, which can read anything the database user can (every center's
// schema, the users table). That's turned off: a `query` is refused, and the
// search box takes a small search language instead, compiled here to SQL from
// known fields only, with every value quoted. It's used wherever listmonk
// searches subscribers: the page, the API, export and the bulk actions.
//
//   john smith            name or address contains "john" and "smith"
//   "john smith"          ... the phrase
//   email:gmail  email=a@b.org  name:smith  domain:gmail.com  domain:yahoo.*
//   status:blocklisted    enabled, disabled or blocklisted
//   list:"Weekly news"    subscribed (not unsubscribed); list:any, list:none
//   confirmed:X  unconfirmed:X  unsubscribed:X   (X a list, or any)
//   created>30d  created<2024-01-01  updated:today   (30d = 30 days ago;
//                         also w, m, y; a date; today, yesterday)
//   opened:"Spring appeal"  opened:any  opened>90d   (a campaign, or when)
//   clicked:X  clicked:https://example.org/page   (a campaign, link or when)
//   bounced:any  bounced:hard  bounced:soft  bounced:complaint  bounced>30d
//   attr.city:hali  attr.city=Halifax  attr.age>=30  attr.address.city=X
//   has:attr.city         the attribute is set and not empty
//   id>1000
//
// Terms are ANDed; OR, NOT (or -term) and parentheses combine them:
//   list:Newsletter -opened>90d     (list:A OR list:B) -status:blocklisted
//
// A list or campaign is its name (any case), or its ID; part of a name is
// enough when only one matches. The views' filter builder writes these
// (assets/js/views/denma-search.js).

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaSearch compiles a subscriber search into an SQL condition for
// listmonk's query: on return *search is empty (the condition does the
// matching) and *query holds the condition. Raw SQL in *query is refused.
func (a *App) denmaSearch(user auth.User, search, query *string) error {
	if strings.TrimSpace(*query) != "" {
		return echo.NewHTTPError(http.StatusBadRequest, "SQL queries are turned off. Search with the search box's filters instead.")
	}
	cond, err := a.denmaSearchSQL(user, *search)
	if err != nil {
		return err
	}
	*search, *query = "", cond
	return nil
}

// denmaSearchSQL is a search's SQL condition ("" for an empty search), or a
// 400 error saying what's wrong with it.
func (a *App) denmaSearchSQL(user auth.User, search string) (string, error) {
	toks, err := denmaLex(search)
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if len(toks) == 0 {
		return "", nil
	}
	p := &denmaParser{a: a, user: user, toks: toks}
	cond, err := p.or()
	if err == nil && p.i < len(p.toks) {
		err = fmt.Errorf("unexpected %s", p.toks[p.i].show())
	}
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, "Search: "+err.Error())
	}
	return "(" + cond + ")", nil
}

// ---- Tokens ----

const (
	denmaTokWord = iota // free text
	denmaTokTerm        // key, op, value
	denmaTokOr
	denmaTokAnd
	denmaTokNot
	denmaTokOpen
	denmaTokClose
)

type denmaTok struct {
	kind          int
	key, op, text string
}

func (t denmaTok) show() string {
	switch t.kind {
	case denmaTokTerm:
		return fmt.Sprintf("%q", t.key+t.op+t.text)
	case denmaTokOr:
		return "OR"
	case denmaTokAnd:
		return "AND"
	case denmaTokNot:
		return "NOT"
	case denmaTokOpen:
		return `"("`
	case denmaTokClose:
		return `")"`
	}
	return fmt.Sprintf("%q", t.text)
}

var denmaTermRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.\-]*)(:|!=|>=|<=|=|>|<)(.*)$`)

func denmaLex(s string) ([]denmaTok, error) {
	var (
		rs  = []rune(s)
		out []denmaTok
		i   int
	)
	quoted := func() string { // at an opening quote
		i++
		start := i
		for i < len(rs) && rs[i] != '"' {
			i++
		}
		v := string(rs[start:i])
		if i < len(rs) {
			i++
		}
		return v
	}
	for i < len(rs) {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(':
			out = append(out, denmaTok{kind: denmaTokOpen})
			i++
		case c == ')':
			out = append(out, denmaTok{kind: denmaTokClose})
			i++
		case c == '"':
			if v := strings.TrimSpace(quoted()); v != "" {
				out = append(out, denmaTok{kind: denmaTokWord, text: v})
			}
		case c == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != ')':
			out = append(out, denmaTok{kind: denmaTokNot})
			i++
		default:
			start := i
			for i < len(rs) && !unicode.IsSpace(rs[i]) && rs[i] != '(' && rs[i] != ')' && rs[i] != '"' {
				i++
			}
			w := string(rs[start:i])
			if m := denmaTermRe.FindStringSubmatch(w); m != nil {
				v := m[3]
				if v == "" && i < len(rs) && rs[i] == '"' {
					v = quoted()
				}
				out = append(out, denmaTok{kind: denmaTokTerm, key: strings.ToLower(m[1]), op: m[2], text: strings.TrimSpace(v)})
				continue
			}
			switch strings.ToUpper(w) {
			case "OR":
				out = append(out, denmaTok{kind: denmaTokOr})
			case "AND":
				out = append(out, denmaTok{kind: denmaTokAnd})
			case "NOT":
				out = append(out, denmaTok{kind: denmaTokNot})
			default:
				out = append(out, denmaTok{kind: denmaTokWord, text: w})
			}
		}
	}
	return out, nil
}

// ---- Parser: or := and (OR and)*; and := not ([AND] not)*; not := (NOT|-) not | ( or ) | term ----

type denmaParser struct {
	a    *App
	user auth.User
	toks []denmaTok
	i    int

	lists, camps []denmaNamed // loaded when first needed
}

type denmaNamed struct {
	ID   int    `db:"id"`
	Name string `db:"name"`
}

func (p *denmaParser) peek() (denmaTok, bool) {
	if p.i < len(p.toks) {
		return p.toks[p.i], true
	}
	return denmaTok{}, false
}

func (p *denmaParser) or() (string, error) {
	parts := []string{}
	for {
		s, err := p.and()
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
		if t, ok := p.peek(); !ok || t.kind != denmaTokOr {
			break
		}
		p.i++
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

func (p *denmaParser) and() (string, error) {
	parts := []string{}
	for {
		t, ok := p.peek()
		if !ok || t.kind == denmaTokOr || t.kind == denmaTokClose {
			break
		}
		if t.kind == denmaTokAnd {
			p.i++
			continue
		}
		s, err := p.not()
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		if t, ok := p.peek(); ok {
			return "", fmt.Errorf("nothing before %s", t.show())
		}
		return "", fmt.Errorf("nothing after the last OR")
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

func (p *denmaParser) not() (string, error) {
	t, _ := p.peek()
	switch t.kind {
	case denmaTokNot:
		p.i++
		if _, ok := p.peek(); !ok {
			return "", fmt.Errorf("nothing after NOT")
		}
		s, err := p.not()
		if err != nil {
			return "", err
		}
		return "NOT COALESCE(" + s + ", FALSE)", nil
	case denmaTokOpen:
		p.i++
		s, err := p.or()
		if err != nil {
			return "", err
		}
		if t, ok := p.peek(); !ok || t.kind != denmaTokClose {
			return "", fmt.Errorf(`missing ")"`)
		}
		p.i++
		return s, nil
	case denmaTokClose, denmaTokOr, denmaTokAnd:
		return "", fmt.Errorf("unexpected %s", t.show())
	}
	p.i++
	if t.kind == denmaTokWord {
		like := denmaLike(t.text)
		return fmt.Sprintf("(subscribers.name ILIKE %s OR subscribers.email ILIKE %s)", like, like), nil
	}
	return p.term(t)
}

// ---- Terms ----

// denmaSearchKeys are the filters, for "unknown filter" errors.
const denmaSearchKeys = "email, name, domain, status, list, confirmed, unconfirmed, unsubscribed, created, updated, opened, clicked, bounced, attr.<name>, has, id"

func (p *denmaParser) term(t denmaTok) (string, error) {
	key, op, v := t.key, t.op, t.text
	if v == "" {
		return "", fmt.Errorf("%s%s needs a value", key, op)
	}
	if op == ":" {
		op = "="
		if key == "email" || key == "name" || strings.HasPrefix(key, "attr.") {
			op = ":" // contains
		}
	}
	bad := func() (string, error) {
		return "", fmt.Errorf("%s can't be used with %s", key, t.op)
	}

	switch {
	case key == "email" || key == "name":
		col := "subscribers." + key
		switch op {
		case ":":
			return col + " ILIKE " + denmaLike(v), nil
		case "=":
			return "LOWER(" + col + ") = LOWER(" + pq.QuoteLiteral(v) + ")", nil
		case "!=":
			return "LOWER(" + col + ") <> LOWER(" + pq.QuoteLiteral(v) + ")", nil
		}
		return bad()

	case key == "domain":
		pat := "%@" + strings.ReplaceAll(denmaLikeEscape(strings.TrimPrefix(v, "@")), "*", "%")
		switch op {
		case "=":
			return "subscribers.email ILIKE " + pq.QuoteLiteral(pat), nil
		case "!=":
			return "subscribers.email NOT ILIKE " + pq.QuoteLiteral(pat), nil
		}
		return bad()

	case key == "status":
		s := strings.ToLower(v)
		if s == "blocked" {
			s = "blocklisted"
		}
		if s != "enabled" && s != "disabled" && s != "blocklisted" {
			return "", fmt.Errorf("status is enabled, disabled or blocklisted, not %q", v)
		}
		switch op {
		case "=":
			return "subscribers.status = '" + s + "'", nil
		case "!=":
			return "subscribers.status <> '" + s + "'", nil
		}
		return bad()

	case key == "id":
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("id is a number, not %q", v)
		}
		if op == "!=" {
			op = "<>"
		}
		return fmt.Sprintf("subscribers.id %s %d", op, n), nil

	case key == "list" || key == "confirmed" || key == "unconfirmed" || key == "unsubscribed":
		status := "dsl.status <> 'unsubscribed'"
		if key != "list" {
			status = "dsl.status = '" + key + "'"
		}
		var cond string
		switch strings.ToLower(v) {
		case "any":
			cond = "EXISTS (SELECT 1 FROM subscriber_lists dsl WHERE dsl.subscriber_id = subscribers.id AND " + status + ")"
		case "none":
			if key != "list" {
				return "", fmt.Errorf("%s:none isn't a filter; try -%s:any", key, key)
			}
			cond = "NOT EXISTS (SELECT 1 FROM subscriber_lists dsl WHERE dsl.subscriber_id = subscribers.id AND " + status + ")"
		default:
			id, err := p.list(v)
			if err != nil {
				return "", err
			}
			cond = fmt.Sprintf("EXISTS (SELECT 1 FROM subscriber_lists dsl WHERE dsl.subscriber_id = subscribers.id AND dsl.list_id = %d AND %s)", id, status)
		}
		switch op {
		case "=":
			return cond, nil
		case "!=":
			return "NOT " + cond, nil
		}
		return bad()

	case key == "created" || key == "updated":
		return denmaDateCond("subscribers."+key+"_at", op, v)

	case key == "opened" || key == "clicked" || key == "bounced":
		return p.activity(key, op, v)

	case key == "has":
		path := strings.TrimPrefix(strings.TrimPrefix(v, "attr."), "attribs.")
		if op != "=" && op != "!=" {
			return bad()
		}
		cond := "COALESCE(" + denmaAttr(path) + ", '') NOT IN ('', 'null', '[]', '{}')"
		if op == "!=" {
			return "NOT " + cond, nil
		}
		return cond, nil

	case strings.HasPrefix(key, "attr.") || strings.HasPrefix(key, "attribs."):
		path := key[strings.Index(key, ".")+1:]
		if path == "" {
			return "", fmt.Errorf("attr. needs an attribute name, like attr.city")
		}
		return denmaAttrCond(denmaAttr(path), denmaAttrJSON(path), op, v)
	}
	return "", fmt.Errorf("unknown filter %q; the filters are %s", t.key+t.op, denmaSearchKeys)
}

// activity is opened, clicked or bounced: by a campaign (or link, or bounce
// type), any, or when.
func (p *denmaParser) activity(key, op, v string) (string, error) {
	table, col := "campaign_views", "dact.campaign_id"
	switch key {
	case "clicked":
		table = "link_clicks"
	case "bounced":
		table = "bounces"
	}
	base := "EXISTS (SELECT 1 FROM " + table + " dact WHERE dact.subscriber_id = subscribers.id"

	switch op {
	case ">", ">=", "<", "<=":
		cond, err := denmaDateCond("dact.created_at", op, v)
		if err != nil {
			return "", err
		}
		return base + " AND " + cond + ")", nil
	case "=", "!=":
	default:
		return "", fmt.Errorf("%s can't be used with %s", key, op)
	}

	var cond string
	lv := strings.ToLower(v)
	switch {
	case lv == "any":
		cond = base + ")"
	case key == "bounced":
		if lv != "hard" && lv != "soft" && lv != "complaint" {
			return "", fmt.Errorf("bounced is any, hard, soft or complaint, not %q", v)
		}
		cond = base + " AND dact.type = '" + lv + "')"
	case key == "clicked" && (strings.Contains(v, "://") || strings.HasPrefix(lv, "www.")):
		cond = base + " AND dact.link_id IN (SELECT id FROM links WHERE url ILIKE " + denmaLike(v) + "))"
	default:
		id, err := p.campaign(v)
		if err != nil {
			return "", err
		}
		cond = fmt.Sprintf("%s AND %s = %d)", base, col, id)
	}
	if op == "!=" {
		return "NOT " + cond, nil
	}
	return cond, nil
}

// ---- Values ----

// denmaLikeEscape escapes LIKE's wildcards.
func denmaLikeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// denmaLike is a quoted "contains" ILIKE pattern.
func denmaLike(s string) string {
	return pq.QuoteLiteral("%" + denmaLikeEscape(s) + "%")
}

// denmaAttr is an attribute's text (a.b.c is attribs' a, then b, then c).
func denmaAttr(path string) string {
	return "(subscribers.attribs #>> " + denmaAttrPath(path) + ")"
}

func denmaAttrJSON(path string) string {
	return "(subscribers.attribs #> " + denmaAttrPath(path) + ")"
}

func denmaAttrPath(path string) string {
	parts := strings.Split(path, ".")
	for i, s := range parts {
		parts[i] = pq.QuoteLiteral(s)
	}
	return "ARRAY[" + strings.Join(parts, ", ") + "]::TEXT[]"
}

var denmaNumRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// denmaAttrCond compares an attribute: contains, equals (or has, in an
// array), or orders numbers, dates or text.
func denmaAttrCond(text, json, op, v string) (string, error) {
	q := pq.QuoteLiteral(v)
	eq := "(LOWER(" + text + ") = LOWER(" + q + ") OR (JSONB_TYPEOF(" + json + ") = 'array' AND " + json + " @> JSONB_BUILD_ARRAY(" + q + "::TEXT)))"
	switch op {
	case ":":
		return text + " ILIKE " + denmaLike(v), nil
	case "=":
		return eq, nil
	case "!=":
		return "NOT COALESCE(" + eq + ", FALSE)", nil
	}
	if denmaNumRe.MatchString(v) {
		return "(CASE WHEN TRIM(" + text + ") ~ '^-?[0-9]+([.][0-9]+)?$' THEN TRIM(" + text + ")::NUMERIC END) " + op + " " + v, nil
	}
	return text + " " + op + " " + q, nil
}

var denmaAgoRe = regexp.MustCompile(`^([0-9]+)\s*(h|d|w|m|y)$`)

// denmaDateCond compares a timestamp column with a date (YYYY-MM-DD, a whole
// day in the database's time zone), today, yesterday, a time (RFC 3339), or
// "N units ago" (30d).
func denmaDateCond(col, op, v string) (string, error) {
	var (
		lv    = strings.ToLower(strings.TrimSpace(v))
		day   string // a date expression, or
		point string // a moment
	)
	switch {
	case lv == "today":
		day = "CURRENT_DATE"
	case lv == "yesterday":
		day = "(CURRENT_DATE - 1)"
	case denmaAgoRe.MatchString(lv):
		m := denmaAgoRe.FindStringSubmatch(lv)
		unit := map[string]string{"h": "hours", "d": "days", "w": "weeks", "m": "months", "y": "years"}[m[2]]
		point = "(NOW() - INTERVAL '" + m[1] + " " + unit + "')"
	default:
		if t, err := time.Parse("2006-01-02", lv); err == nil {
			day = "'" + t.Format("2006-01-02") + "'::DATE"
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			point = "'" + t.UTC().Format(time.RFC3339Nano) + "'::TIMESTAMPTZ"
		} else {
			return "", fmt.Errorf("%q isn't a date; use 2024-01-31, today, yesterday or 30d (30 days ago; also h, w, m, y)", v)
		}
	}

	if point != "" {
		switch op {
		case ">", ">=", "<", "<=":
			return col + " " + op + " " + point, nil
		}
		return "", fmt.Errorf("%q is a moment; compare it with > or <", v)
	}
	next := "(" + day + " + 1)"
	switch op {
	case "=":
		return col + " >= " + day + " AND " + col + " < " + next, nil
	case "!=":
		return "NOT (" + col + " >= " + day + " AND " + col + " < " + next + ")", nil
	case ">":
		return col + " >= " + next, nil
	case ">=":
		return col + " >= " + day, nil
	case "<":
		return col + " < " + day, nil
	case "<=":
		return col + " < " + next, nil
	}
	return "", fmt.Errorf("dates can't be used with %s", op)
}

// list and campaign find a list (the user can see) or campaign by name or ID.
func (p *denmaParser) list(v string) (int, error) {
	if p.lists == nil {
		p.lists = []denmaNamed{}
		var all []denmaNamed
		if err := p.a.db.Select(&all, `SELECT id, name FROM lists ORDER BY id`); err != nil {
			return 0, err
		}
		hasAll, ids := p.user.GetPermittedLists(auth.PermTypeGet | auth.PermTypeManage)
		ok := map[int]bool{}
		for _, id := range ids {
			ok[id] = true
		}
		for _, l := range all {
			if hasAll || ok[l.ID] {
				p.lists = append(p.lists, l)
			}
		}
	}
	return denmaFind(p.lists, v, "list")
}

func (p *denmaParser) campaign(v string) (int, error) {
	if p.camps == nil {
		p.camps = []denmaNamed{}
		if err := p.a.db.Select(&p.camps, `SELECT id, name FROM campaigns ORDER BY id`); err != nil {
			return 0, err
		}
	}
	return denmaFind(p.camps, v, "campaign")
}

// denmaFind is the one named v (any case), with ID v, or whose name contains v.
func denmaFind(all []denmaNamed, v, what string) (int, error) {
	lv := strings.ToLower(v)
	for _, x := range all {
		if strings.ToLower(x.Name) == lv {
			return x.ID, nil
		}
	}
	if n, err := strconv.Atoi(v); err == nil {
		for _, x := range all {
			if x.ID == n {
				return x.ID, nil
			}
		}
	}
	var found []denmaNamed
	for _, x := range all {
		if strings.Contains(strings.ToLower(x.Name), lv) {
			found = append(found, x)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("there's no %s called %q", what, v)
	case 1:
		return found[0].ID, nil
	}
	names := []string{}
	for i, x := range found {
		if i == 3 {
			names = append(names, fmt.Sprintf("and %d more", len(found)-3))
			break
		}
		names = append(names, fmt.Sprintf("%q", x.Name))
	}
	return 0, fmt.Errorf("%q matches several %ss (%s); use the whole name", v, what, strings.Join(names, ", "))
}

// ---- API ----

func initDenmaSearchAPIHandlers(g *echo.Group, a *App) {
	g.GET("/api/denma/search/check", a.auth.Perm(a.DenmaCheckSearch, "subscribers:get_all", "subscribers:get"))
	g.GET("/api/denma/stats/subscribers", a.auth.Perm(a.DenmaStatSubscribers, "subscribers:get_all", "subscribers:get"))
}

// DenmaCheckSearch says whether a search is valid (the search box checks
// before it searches, so a mistake is a message rather than an error page).
func (a *App) DenmaCheckSearch(c echo.Context) error {
	if _, err := a.denmaSearchSQL(auth.GetUser(c), c.QueryParam("search")); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{true})
}
