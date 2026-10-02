package main

// denma: search for subscribers, lists and campaigns. listmonk's advanced
// subscriber search takes a raw SQL expression, which can read anything the
// database user can (every center's schema, the users table). That's turned
// off: a `query` is refused, and the search boxes take a small search language
// instead, compiled here to SQL from known fields only, with every value
// quoted. The lists and campaigns pages had only a plain search; they take the
// same language, with their own fields.
//
// Plain words match names (and, for subscribers, addresses; for campaigns,
// subjects). Terms are ANDed; OR, NOT (or -term) and parentheses combine them:
//   list:Newsletter -opened>90d     (status:draft OR status:paused) tag:retreat
// ":" is "contains" for text and "is" for the rest; =, !=, >, >=, < and <=
// also work where they make sense. Values with spaces go in quotes. A date is
// 2024-01-31 (the whole day), today, yesterday, or 30d for 30 days ago (also
// h, w, m and y): created>30d is "in the last 30 days"; scheduled:any is set. A list, campaign or
// template is its name (any case) or ID; part of a name is enough when only
// one matches.
//
// Subscribers: email, name, domain (domain:yahoo.*), status, list, confirmed,
//   unconfirmed, unsubscribed (a list, any, or list:none), created, updated,
//   opened, clicked (a campaign, a link, any, or when), bounced (hard, soft,
//   complaint, any, or when), attr.<name> (attr.address.city for nested),
//   has:attr.<name>, id.
// Lists: name, type, optin, status, tag, subscribers (a count), campaign (sent
//   to by a campaign, or any), mailed (when a campaign last started to it, or
//   any), created, updated, id.
// Campaigns: name, subject, from, status, type, format, tag, list, template,
//   created, updated, started, scheduled, sent, opens, clicks, bounces (counts),
//   archive (yes or no), id.
//
// The views' filter builder writes these (assets/js/views/denma-search.js).

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// What's searched, and how its table is named in the conditions.
type denmaKind string

const (
	denmaSubscribers denmaKind = "subscribers" // listmonk's own queries: subscribers
	denmaLists       denmaKind = "lists"       // ours: lists l
	denmaCampaigns   denmaKind = "campaigns"   // listmonk's query-campaigns: campaigns c
)

var denmaSearchKeys = map[denmaKind]string{
	denmaSubscribers: "email, name, domain, status, list, confirmed, unconfirmed, unsubscribed, created, updated, opened, clicked, bounced, attr.<name>, has, id",
	denmaLists:       "name, type, optin, status, tag, subscribers, campaign, mailed, created, updated, id",
	denmaCampaigns:   "name, subject, from, status, type, format, tag, list, template, created, updated, started, scheduled, sent, opens, clicks, bounces, archive, id",
}

// denmaSearch compiles a subscriber search into an SQL condition for
// listmonk's query: on return *search is empty (the condition does the
// matching) and *query holds the condition. Raw SQL in *query is refused.
func (a *App) denmaSearch(user auth.User, search, query *string) error {
	if strings.TrimSpace(*query) != "" {
		return echo.NewHTTPError(http.StatusBadRequest, "SQL queries are turned off. Search with the search box's filters instead.")
	}
	cond, err := a.denmaSearchSQL(denmaSubscribers, user, *search)
	if err != nil {
		return err
	}
	*search, *query = "", cond
	return nil
}

// denmaListSearch narrows the lists a lists query may return to those that
// match a search: on return *search is empty, and, if there was one, *all is
// false and *permitted holds the matching lists the user may see.
func (a *App) denmaListSearch(user auth.User, search *string, all *bool, permitted *[]int) error {
	cond, err := a.denmaSearchSQL(denmaLists, user, *search)
	if err != nil || cond == "" {
		*search = ""
		return err
	}
	ids := []int{}
	if err := a.db.Select(&ids, `SELECT l.id FROM lists l WHERE `+cond+` AND ($1 OR l.id = ANY($2::INT[]))`, *all, pq.Array(*permitted)); err != nil {
		a.log.Printf("denma: error searching lists: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error searching lists: "+err.Error())
	}
	*search, *all, *permitted = "", false, ids
	return nil
}

// denmaQueryCampaigns is listmonk's QueryCampaigns with the search language.
func (a *App) denmaQueryCampaigns(user auth.User, search string, statuses, tags []string, typ, orderBy, order string, getAll bool, permittedLists []int, offset, limit int) (models.Campaigns, int, error) {
	cond, err := a.denmaSearchSQL(denmaCampaigns, user, search)
	if err != nil {
		return nil, 0, err
	}
	if cond == "" {
		return a.core.QueryCampaigns("", statuses, tags, typ, orderBy, order, getAll, permittedLists, offset, limit)
	}
	return a.core.DenmaQueryCampaigns(cond, statuses, tags, typ, orderBy, order, getAll, permittedLists, offset, limit)
}

// denmaCampaignDeletes turns a bulk delete's search into the campaigns that
// match it: on return *query is empty and *ids holds them (0 if none, which
// deletes nothing). listmonk still checks the user's permission for each.
func (a *App) denmaCampaignDeletes(user auth.User, ids *[]int, query *string) error {
	if len(*ids) > 0 || strings.TrimSpace(*query) == "" {
		return nil
	}
	cond, err := a.denmaSearchSQL(denmaCampaigns, user, *query)
	if err != nil {
		return err
	}
	found := []int{}
	if err := a.db.Select(&found, `SELECT c.id FROM campaigns c WHERE `+cond); err != nil {
		a.log.Printf("denma: error searching campaigns: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error searching campaigns: "+err.Error())
	}
	if len(found) == 0 {
		found = []int{0}
	}
	*ids, *query = found, ""
	return nil
}

// denmaSearchSQL is a search's SQL condition ("" for an empty search), or a
// 400 error saying what's wrong with it.
func (a *App) denmaSearchSQL(kind denmaKind, user auth.User, search string) (string, error) {
	toks := denmaLex(search)
	if len(toks) == 0 {
		return "", nil
	}
	p := &denmaParser{a: a, kind: kind, user: user, toks: toks}
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

func denmaLex(s string) []denmaTok {
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
				// "Last call: Autumn": a key with nothing after it is just a word.
				if v != "" || m[2] != ":" {
					out = append(out, denmaTok{kind: denmaTokTerm, key: strings.ToLower(m[1]), op: m[2], text: strings.TrimSpace(v)})
					continue
				}
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
	return out
}

// ---- Parser: or := and (OR and)*; and := not ([AND] not)*; not := (NOT|-) not | ( or ) | term ----

type denmaParser struct {
	a    *App
	kind denmaKind
	user auth.User
	toks []denmaTok
	i    int

	lists, camps, tpls []denmaNamed // loaded when first needed
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
		switch p.kind {
		case denmaLists:
			return "l.name ILIKE " + like, nil
		case denmaCampaigns:
			return "(c.name ILIKE " + like + " OR c.subject ILIKE " + like + ")", nil
		}
		return "(subscribers.name ILIKE " + like + " OR subscribers.email ILIKE " + like + ")", nil
	}

	if t.text == "" {
		return "", fmt.Errorf("%s%s needs a value", t.key, t.op)
	}
	var (
		cond string
		err  error
	)
	switch p.kind {
	case denmaLists:
		cond, err = p.listTerm(t.key, t.op, t.text)
	case denmaCampaigns:
		cond, err = p.campaignTerm(t.key, t.op, t.text)
	default:
		cond, err = p.subscriberTerm(t.key, t.op, t.text)
	}
	if err == nil && cond == "" {
		err = fmt.Errorf("unknown filter %q; the filters are %s. To search for text with a colon, put it in quotes", t.key+t.op, denmaSearchKeys[p.kind])
	}
	return cond, err
}

// ---- Terms. Each returns "" for a key it doesn't know. ----

func (p *denmaParser) subscriberTerm(key, op, v string) (string, error) {
	switch {
	case key == "email" || key == "name":
		return denmaText("subscribers."+key, key, op, v)

	case key == "domain":
		pat := pq.QuoteLiteral("%@" + strings.ReplaceAll(denmaLikeEscape(strings.TrimPrefix(v, "@")), "*", "%"))
		return denmaIs(key, op, "subscribers.email ILIKE "+pat)

	case key == "status":
		if strings.EqualFold(v, "blocked") {
			v = "blocklisted"
		}
		return denmaEnum("subscribers.status", key, op, v, "enabled", "disabled", "blocklisted")

	case key == "id":
		return denmaNum("subscribers.id", key, op, v)

	case key == "list" || key == "confirmed" || key == "unconfirmed" || key == "unsubscribed":
		status := "dsl.status <> 'unsubscribed'"
		if key != "list" {
			status = "dsl.status = '" + key + "'"
		}
		sub := "EXISTS (SELECT 1 FROM subscriber_lists dsl WHERE dsl.subscriber_id = subscribers.id AND " + status
		switch strings.ToLower(v) {
		case "any":
			return denmaIs(key, op, sub+")")
		case "none":
			if key != "list" {
				return "", fmt.Errorf("%s:none isn't a filter; try -%s:any", key, key)
			}
			return denmaIs(key, op, "NOT "+sub+")")
		}
		id, err := p.list(v)
		if err != nil {
			return "", err
		}
		return denmaIs(key, op, fmt.Sprintf("%s AND dsl.list_id = %d)", sub, id))

	case key == "created" || key == "updated":
		return denmaDateCond("subscribers."+key+"_at", op, v)

	case key == "opened" || key == "clicked" || key == "bounced":
		return p.activity(key, op, v)

	case key == "has":
		path := strings.TrimPrefix(strings.TrimPrefix(v, "attr."), "attribs.")
		return denmaIs(key, op, "COALESCE("+denmaAttr(path)+", '') NOT IN ('', 'null', '[]', '{}')")

	case strings.HasPrefix(key, "attr.") || strings.HasPrefix(key, "attribs."):
		path := key[strings.Index(key, ".")+1:]
		if path == "" {
			return "", fmt.Errorf("attr. needs an attribute name, like attr.city")
		}
		return denmaAttrCond(denmaAttr(path), denmaAttrJSON(path), op, v)
	}
	return "", nil
}

func (p *denmaParser) listTerm(key, op, v string) (string, error) {
	switch key {
	case "name":
		return denmaText("l.name", key, op, v)
	case "type":
		return denmaEnum("l.type", key, op, v, "public", "private", "temporary")
	case "optin":
		return denmaEnum("l.optin", key, op, v, "single", "double")
	case "status":
		return denmaEnum("l.status", key, op, v, "active", "archived")
	case "tag":
		return denmaTag("l.tags", key, op, v)
	case "subscribers":
		return denmaNum("(SELECT COUNT(*) FROM subscriber_lists dsl WHERE dsl.list_id = l.id AND dsl.status <> 'unsubscribed')", key, op, v)
	case "created", "updated":
		return denmaDateCond("l."+key+"_at", op, v)
	case "id":
		return denmaNum("l.id", key, op, v)

	case "campaign":
		sub := "EXISTS (SELECT 1 FROM campaign_lists dcl WHERE dcl.list_id = l.id"
		if strings.EqualFold(v, "any") {
			return denmaIs(key, op, sub+")")
		}
		id, err := p.campaign(v)
		if err != nil {
			return "", err
		}
		return denmaIs(key, op, fmt.Sprintf("%s AND dcl.campaign_id = %d)", sub, id))

	case "mailed":
		sub := "EXISTS (SELECT 1 FROM campaign_lists dcl JOIN campaigns dc ON dc.id = dcl.campaign_id WHERE dcl.list_id = l.id AND dc.started_at IS NOT NULL"
		if strings.EqualFold(v, "any") {
			return denmaIs(key, op, sub+")")
		}
		cond, err := denmaDateCond("dc.started_at", op, v)
		if err != nil {
			return "", err
		}
		return sub + " AND " + cond + ")", nil
	}
	return "", nil
}

func (p *denmaParser) campaignTerm(key, op, v string) (string, error) {
	switch key {
	case "name", "subject":
		return denmaText("c."+key, key, op, v)
	case "from":
		return denmaText("c.from_email", key, op, v)
	case "status":
		return denmaEnum("c.status", key, op, v, "draft", "scheduled", "running", "paused", "cancelled", "finished")
	case "type":
		return denmaEnum("c.type", key, op, v, "regular", "optin")
	case "format":
		return denmaEnum("c.content_type", key, op, v, "richtext", "html", "markdown", "plain", "visual")
	case "tag":
		return denmaTag("c.tags", key, op, v)
	case "created", "updated":
		return denmaDateCond("c."+key+"_at", op, v)
	case "started":
		return denmaDateCond("c.started_at", op, v)
	case "scheduled":
		return denmaDateCond("c.send_at", op, v)
	case "sent":
		return denmaNum("c.sent", key, op, v)
	case "opens":
		return denmaNum("(SELECT COUNT(*) FROM campaign_views dv WHERE dv.campaign_id = c.id)", key, op, v)
	case "clicks":
		return denmaNum("(SELECT COUNT(*) FROM link_clicks dk WHERE dk.campaign_id = c.id)", key, op, v)
	case "bounces":
		return denmaNum("(SELECT COUNT(*) FROM bounces db WHERE db.campaign_id = c.id)", key, op, v)
	case "id":
		return denmaNum("c.id", key, op, v)

	case "archive":
		switch strings.ToLower(v) {
		case "yes", "true":
			return denmaIs(key, op, "c.archive")
		case "no", "false":
			return denmaIs(key, op, "NOT c.archive")
		}
		return "", fmt.Errorf("archive is yes or no, not %q", v)

	case "list":
		sub := "EXISTS (SELECT 1 FROM campaign_lists dcl WHERE dcl.campaign_id = c.id"
		if strings.EqualFold(v, "any") {
			return denmaIs(key, op, sub+")")
		}
		id, err := p.list(v)
		if err != nil {
			return "", err
		}
		return denmaIs(key, op, fmt.Sprintf("%s AND dcl.list_id = %d)", sub, id))

	case "template":
		id, err := p.template(v)
		if err != nil {
			return "", err
		}
		return denmaIs(key, op, fmt.Sprintf("c.template_id = %d", id))
	}
	return "", nil
}

// activity is a subscriber's opens, clicks or bounces: by a campaign (or
// link, or bounce type), any, or when.
func (p *denmaParser) activity(key, op, v string) (string, error) {
	table := map[string]string{"opened": "campaign_views", "clicked": "link_clicks", "bounced": "bounces"}[key]
	base := "EXISTS (SELECT 1 FROM " + table + " dact WHERE dact.subscriber_id = subscribers.id"

	switch op {
	case ">", ">=", "<", "<=":
		cond, err := denmaDateCond("dact.created_at", op, v)
		if err != nil {
			return "", err
		}
		return base + " AND " + cond + ")", nil
	}

	lv := strings.ToLower(v)
	switch {
	case lv == "any":
		return denmaIs(key, op, base+")")
	case key == "bounced":
		if lv != "hard" && lv != "soft" && lv != "complaint" {
			return "", fmt.Errorf("bounced is any, hard, soft or complaint, not %q", v)
		}
		return denmaIs(key, op, base+" AND dact.type = '"+lv+"')")
	case key == "clicked" && (strings.Contains(v, "://") || strings.HasPrefix(lv, "www.")):
		return denmaIs(key, op, base+" AND dact.link_id IN (SELECT id FROM links WHERE url ILIKE "+denmaLike(v)+"))")
	}
	id, err := p.campaign(v)
	if err != nil {
		return "", err
	}
	return denmaIs(key, op, fmt.Sprintf("%s AND dact.campaign_id = %d)", base, id))
}

// ---- Comparisons ----

// denmaIs is cond for : or =, and its opposite for !=.
func denmaIs(key, op, cond string) (string, error) {
	switch op {
	case ":", "=":
		return cond, nil
	case "!=":
		return "NOT COALESCE(" + cond + ", FALSE)", nil
	}
	return "", fmt.Errorf("%s can't be used with %s", key, op)
}

// denmaText is a text column: contains (:), is (=) or isn't (!=), in any case.
func denmaText(col, key, op, v string) (string, error) {
	if op == ":" {
		return col + " ILIKE " + denmaLike(v), nil
	}
	return denmaIs(key, op, "LOWER("+col+") = LOWER("+pq.QuoteLiteral(v)+")")
}

// denmaEnum is a column with one of a few values.
func denmaEnum(col, key, op, v string, allowed ...string) (string, error) {
	lv := strings.ToLower(v)
	for _, s := range allowed {
		if s == lv {
			return denmaIs(key, op, col+" = '"+s+"'")
		}
	}
	return "", fmt.Errorf("%s is %s, not %q", key, strings.Join(allowed, ", "), v)
}

// denmaNum compares a whole number.
func denmaNum(expr, key, op, v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return "", fmt.Errorf("%s is a number, not %q", key, v)
	}
	switch op {
	case ":", "=":
		op = "="
	case "!=":
		op = "<>"
	}
	return fmt.Sprintf("%s %s %d", expr, op, n), nil
}

// denmaTag is a tags array having a tag (any case).
func denmaTag(col, key, op, v string) (string, error) {
	return denmaIs(key, op, "EXISTS (SELECT 1 FROM UNNEST("+col+") dt WHERE LOWER(dt) = LOWER("+pq.QuoteLiteral(v)+"))")
}

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
	switch op {
	case ":":
		return text + " ILIKE " + denmaLike(v), nil
	case "=", "!=":
		return denmaIs("attr", op, "(LOWER("+text+") = LOWER("+q+") OR (JSONB_TYPEOF("+json+") = 'array' AND "+json+" @> JSONB_BUILD_ARRAY("+q+"::TEXT)))")
	}
	if denmaNumRe.MatchString(v) {
		return "(CASE WHEN TRIM(" + text + ") ~ '^-?[0-9]+([.][0-9]+)?$' THEN TRIM(" + text + ")::NUMERIC END) " + op + " " + v, nil
	}
	return text + " " + op + " " + q, nil
}

var denmaAgoRe = regexp.MustCompile(`^([0-9]+)\s*(h|d|w|m|y)$`)

// denmaDateCond compares a timestamp column with a date (YYYY-MM-DD, a whole
// day in the database's time zone), today, yesterday, a time (RFC 3339), or
// "N units ago" (30d); or, with any, checks that it's set.
func denmaDateCond(col, op, v string) (string, error) {
	var (
		lv    = strings.ToLower(strings.TrimSpace(v))
		day   string // a date expression, or
		point string // a moment
	)
	switch {
	case lv == "any": // set at all (a campaign that's scheduled, started)
		return denmaIs("the date", op, col+" IS NOT NULL")
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
	case ":", "=":
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

// ---- Names ----

// list, campaign and template find a list (one the user can see), campaign
// or template by name or ID.
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

func (p *denmaParser) template(v string) (int, error) {
	if p.tpls == nil {
		p.tpls = []denmaNamed{}
		if err := p.a.db.Select(&p.tpls, `SELECT id, name FROM templates ORDER BY id`); err != nil {
			return 0, err
		}
	}
	return denmaFind(p.tpls, v, "template")
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
	g.GET("/api/denma/search/check", a.DenmaCheckSearch)
	g.GET("/api/denma/stats/subscribers", a.auth.Perm(a.DenmaStatSubscribers, "subscribers:get_all", "subscribers:get"))
}

// DenmaCheckSearch says whether a search (?kind=subscribers, lists or
// campaigns) is valid: the search boxes check before they search, so a
// mistake is a message rather than an error page. Only names the user could
// search for anyway are looked up.
func (a *App) DenmaCheckSearch(c echo.Context) error {
	user := auth.GetUser(c)
	kind := denmaKind(c.QueryParam("kind"))
	perms := map[denmaKind][]string{
		denmaSubscribers: {"subscribers:get_all", "subscribers:get"},
		denmaLists:       {"lists:get_all", "lists:manage_all"},
		denmaCampaigns:   {"campaigns:get_all", "campaigns:get", "campaigns:manage_all", "campaigns:manage"},
	}[kind]
	if perms == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "kind is subscribers, lists or campaigns")
	}
	allowed := kind == denmaLists // anyone with a list role may search lists
	for _, perm := range perms {
		allowed = allowed || user.HasPerm(perm)
	}
	if !allowed {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", perms[0]))
	}
	if _, err := a.denmaSearchSQL(kind, user, c.QueryParam("search")); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{true})
}
