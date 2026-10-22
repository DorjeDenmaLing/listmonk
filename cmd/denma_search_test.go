package main

// The search language's compiler (cmd/denma_search.go). Values are never
// passed as arguments: each is quoted into the SQL with pq.QuoteLiteral. So
// these check that, whatever is typed, the SQL outside its quoted literals is
// only the compiler's own, and that mistakes are errors.

import (
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

// denmaTestCompile is denmaSearchSQL with the names it looks up given, so no
// database is needed.
func denmaTestCompile(kind denmaKind, search string) (string, error) {
	toks := denmaLex(search)
	if len(toks) == 0 {
		return "", nil
	}
	p := &denmaParser{kind: kind, toks: toks,
		lists: []denmaNamed{{1, "Newsletter"}, {2, "News digest"}, {3, "Retreats"}},
		camps: []denmaNamed{{7, "Spring appeal"}, {8, "Spring retreat"}, {9, "Autumn"}},
		tpls:  []denmaNamed{{4, "Default"}},
	}
	cond, err := p.or()
	if err == nil && p.i < len(p.toks) {
		err = errors.New("unexpected " + p.toks[p.i].show())
	}
	if err != nil {
		return "", err
	}
	return "(" + cond + ")", nil
}

// denmaSplitSQL separates SQL into its code (each literal as ?) and its
// literals' values, as Postgres reads them: a doubled quote is a quote, and in E'...' a
// backslash escapes the next character. ok is false if a literal isn't closed.
func denmaSplitSQL(s string) (code string, lits []string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		// E'...' (the E a word of its own) is an escape string.
		esc := false
		if c := b.String(); strings.HasSuffix(c, "E") || strings.HasSuffix(c, "e") {
			if n := len(c) - 1; n == 0 || strings.ContainsRune(" (,", rune(c[n-1])) {
				esc = true
				b.Reset()
				b.WriteString(c[:n])
			}
		}
		var v strings.Builder
		closed := false
		for i++; i < len(s); i++ {
			switch {
			case esc && s[i] == '\\' && i+1 < len(s):
				i++
				v.WriteByte(s[i])
			case s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
				i++
				v.WriteByte('\'')
			case s[i] == '\'':
				closed = true
			default:
				v.WriteByte(s[i])
			}
			if closed {
				break
			}
		}
		if !closed {
			return b.String(), lits, false
		}
		lits = append(lits, v.String())
		b.WriteByte('?')
	}
	return b.String(), lits, true
}

// denmaSafeSQL is why a compiled condition isn't safe ("" if it is): an open
// literal, unbalanced brackets, or anything of the user's outside literals.
func denmaSafeSQL(sql string) string {
	code, _, ok := denmaSplitSQL(sql)
	if !ok {
		return "a literal isn't closed"
	}
	depth := 0
	for _, c := range code {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth < 0 {
			return "a bracket closes too soon"
		}
	}
	if depth != 0 {
		return "brackets don't balance"
	}
	for _, bad := range []string{"PWNED", ";", "--", "/*", "*/", "$", `\`, `"`, "\x00"} {
		if strings.Contains(strings.ToUpper(code), bad) {
			return "the SQL text has " + bad + ": " + code
		}
	}
	return ""
}

// Hostile values, each with the canary PWNED, which must never be outside a
// literal.
var denmaHostile = []string{
	`PWNED`,
	`PWNED'`,
	`'PWNED`,
	`' OR '1'='1' -- PWNED`,
	`'; DROP TABLE subscribers; -- PWNED`,
	`\'; PWNED --`,
	`\\'PWNED`,
	`PWNED\`,
	`PWNED\\`,
	`E'PWNED`,
	`e'\'PWNED`,
	`$$PWNED$$`,
	`$1 PWNED $2`,
	`/* PWNED */`,
	`PWNED%_*`,
	`%query% PWNED %order%`,
	`') OR (SELECT pg_sleep(5)) IS NULL OR ('PWNED`,
	`x' UNION SELECT email FROM center_halifax.subscribers --PWNED`,
	`center_halifax.subscribers.PWNED`,
	`a.b'c.PWNED`,
	`PWNED'::TEXT||'`,
	`''''PWNED''''`,
	`PWNED` + "\n" + `'; SELECT 1`,
}

// Every hostile value, in every field that takes text, is only ever inside a
// literal, and a field that compares it whole gets it back as typed.
func TestDenmaSearchValuesQuoted(t *testing.T) {
	fields := map[denmaKind][]string{
		denmaSubscribers: {`name:"%s"`, `name="%s"`, `name!="%s"`, `email:"%s"`, `email="%s"`, `domain:"%s"`, `"%s"`,
			`attr.city:"%s"`, `attr.city="%s"`, `attr.city!="%s"`, `attr.city>"%s"`, `attr.city<="%s"`, `attr.a.b="%s"`,
			`has:"attr.%s"`, `has:"%s"`, `tag:"%s"`, `clicked:"https://%s"`, `-name:"%s" OR "%s"`},
		denmaLists:     {`name:"%s"`, `name="%s"`, `"%s"`, `-name:"%s"`},
		denmaCampaigns: {`name:"%s"`, `subject="%s"`, `from:"%s"`, `tag:"%s"`, `"%s"`, `(name="%s" OR subject!="%s")`},
	}
	for kind, fs := range fields {
		for _, f := range fs {
			for _, v := range denmaHostile {
				search := strings.ReplaceAll(f, "%s", v)
				sql, err := denmaTestCompile(kind, search)
				if err != nil {
					// A tag with , or | in it is several tags.
					if !strings.HasPrefix(f, "tag:") || !strings.Contains(err.Error(), "needs one tag") {
						t.Errorf("%s %s: %v", kind, search, err)
					}
					continue
				}
				if why := denmaSafeSQL(sql); why != "" {
					t.Errorf("%s %s: %s\n%s", kind, search, why, sql)
				}
				if strings.Contains(f, `="%s"`) && !strings.HasPrefix(f, "has") {
					_, lits, _ := denmaSplitSQL(sql)
					found := false
					for _, l := range lits {
						found = found || l == strings.TrimSpace(v)
					}
					if !found {
						t.Errorf("%s %s: the value isn't one of the literals %q", kind, search, lits)
					}
				}
			}
		}
	}
}

// Whole searches as typed, with quotes, keys and operators in odd places:
// each is an error or safe SQL, in every kind.
func TestDenmaSearchHostileSearches(t *testing.T) {
	searches := []string{
		`'`, `\`, `\'`, `''`, `"`, `"'"`, `'"'`,
		`x' OR '1'='1`, `PWNED' OR '1'='1`, `name:PWNED'--`, `email:\' OR TRUE --PWNED`,
		`a; SELECT PWNED`, `/* PWNED */`, `-- PWNED`, `PWNED --`,
		`attr.a'b:PWNED`, `attr.a.b'c:PWNED`, `attr.city') OR ('1'='1:PWNED`, `attr.ci"ty:PWNED`,
		`has:"attr.x') OR TRUE --PWNED"`, `has:attr.a.b'PWNED`,
		`attr.age>1 OR 1=1`, `attr.age>"0' OR 'PWNED'='PWNED"`, `attr.age>1e400`, `attr.age>-1`, `attr.age>=1.5`,
		`list:"x' OR '1'='1"`, `id:1 OR 1=1`, `id:1;PWNED`, `id:0x10`, `id:1' OR 'PWNED`, `id:-1 UNION SELECT PWNED`,
		`created>2024-01-01' OR 'PWNED`, `created>30d' OR 'PWNED`, `created:"2024-01-01'::DATE OR PWNED --"`,
		`created>2024-01-31T10:00:00+02:00`, `created>"2024-01-31T10:00:00Z' OR 'PWNED"`,
		`UNION SELECT email FROM center_halifax.subscribers PWNED`, `center_halifax.subscribers.email:PWNED`,
		`pg_sleep(5)`, `name:"'||pg_sleep(5)||'PWNED"`, `x');SELECT pg_sleep(5);--PWNED`,
		`status:"enabled' OR 'PWNED"`, `type:"public' OR 'PWNED"`, `optin:"double'--PWNED"`, `format:"html'PWNED"`,
		`archive:"yes' OR 'PWNED"`, `bounced:"hard' OR 'PWNED"`, `subscribers>"1 OR PWNED"`, `opens>1;PWNED`,
		`$1 $$PWNED$$ %query% %order%`, `name=%query%PWNED`, `(((PWNED)))`, `NOT NOT -PWNED`,
		`"PWNED` + "\x00" + `'"`, "name:PWNED\t'x", `name:"PWNED` + "\n" + `'; SELECT 1"`,
	}
	for _, kind := range []denmaKind{denmaSubscribers, denmaLists, denmaCampaigns} {
		for _, s := range searches {
			sql, err := denmaTestCompile(kind, s)
			if err != nil {
				continue
			}
			if why := denmaSafeSQL(sql); why != "" {
				t.Errorf("%s %s: %s\n%s", kind, s, why, sql)
			}
		}
	}
}

// Random searches from keys, operators and hostile pieces never panic, and
// are always an error or safe SQL.
func TestDenmaSearchFuzz(t *testing.T) {
	keys := []string{"", "", "name", "email", "domain", "status", "list", "unsubscribed", "created", "opened", "clicked",
		"bounced", "tag", "attr.city", "attr.a.b", "has", "id", "type", "optin", "subscribers", "campaign", "mailed",
		"subject", "from", "format", "template", "archive", "opens", "scheduled", "nope", "attr."}
	ops := []string{":", "=", "!=", ">", ">=", "<", "<="}
	bits := []string{"PWNED", "'", `\`, `"`, " ", "(", ")", ";", "--", "/*", "$$", "%", "_", "*", ".", ":", "-", "E'",
		"x", "5", "-1", "2024-01-31", "30d", "any", "none", "OR", "1=1", "Newsletter", "Spring", "\x00", "hard", "yes"}
	r := rand.New(rand.NewSource(1))
	pick := func(s []string) string { return s[r.Intn(len(s))] }
	for n := 0; n < 20000; n++ {
		var b strings.Builder
		for terms := r.Intn(4) + 1; terms > 0; terms-- {
			switch r.Intn(6) {
			case 0:
				b.WriteString("-")
			case 1:
				b.WriteString("(")
			case 2:
				b.WriteString(" OR ")
			}
			if k := pick(keys); k != "" {
				b.WriteString(k + pick(ops))
			}
			for v := r.Intn(4) + 1; v > 0; v-- {
				b.WriteString(pick(bits))
			}
			if r.Intn(5) == 0 {
				b.WriteString(")")
			}
			b.WriteString(" ")
		}
		s := b.String()
		for _, kind := range []denmaKind{denmaSubscribers, denmaLists, denmaCampaigns} {
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("%s %q: panic %v", kind, s, p)
					}
				}()
				if sql, err := denmaTestCompile(kind, s); err == nil {
					if why := denmaSafeSQL(sql); why != "" {
						t.Errorf("%s %q: %s\n%s", kind, s, why, sql)
					}
				}
			}()
		}
	}
}

// Unknown fields and malformed searches are errors, each with a message.
func TestDenmaSearchErrors(t *testing.T) {
	for _, c := range []struct {
		kind         denmaKind
		search, want string
	}{
		{denmaSubscribers, "colour:red", "unknown filter"},
		{denmaSubscribers, "center_halifax.subscribers.email:x", "unknown filter"},
		{denmaSubscribers, "l.id>0", "unknown filter"},
		{denmaSubscribers, "subscribers.id>0", "unknown filter"},
		{denmaSubscribers, "type:public", "unknown filter"},
		{denmaLists, "email:x", "unknown filter"},
		{denmaLists, "has:attr.city", "unknown filter"},
		{denmaLists, "attr.city:x", "unknown filter"},
		{denmaCampaigns, "domain:x", "unknown filter"},
		{denmaCampaigns, "campaign:x", "unknown filter"},
		{denmaSubscribers, "(a", `missing ")"`},
		{denmaSubscribers, "a)", `unexpected ")"`},
		{denmaSubscribers, ")", `nothing before ")"`},
		{denmaSubscribers, "OR", "nothing before OR"},
		{denmaSubscribers, "a OR", "nothing after the last OR"},
		{denmaSubscribers, "NOT", "nothing after NOT"},
		{denmaSubscribers, "a AND", ""},
		{denmaSubscribers, "()", "nothing before"},
		{denmaSubscribers, "name=", "needs a value"},
		{denmaSubscribers, "attr.:x", "attribute name"},
		{denmaSubscribers, "id:1x", "is a number"},
		{denmaSubscribers, "id:0x10", "is a number"},
		{denmaSubscribers, "id:1.5", "is a number"},
		{denmaSubscribers, "id:99999999999999999999", "is a number"},
		{denmaSubscribers, "status:foo", "status is enabled"},
		{denmaSubscribers, "status>enabled", "can't be used with >"},
		{denmaSubscribers, "domain>x", "can't be used with >"},
		{denmaSubscribers, "created>2024-13-45", "isn't a date"},
		{denmaSubscribers, "created>last-week", "isn't a date"},
		{denmaSubscribers, "created:30d", "is a moment"},
		{denmaSubscribers, "created>99999999999999999999d", "too long ago"},
		{denmaSubscribers, "created>1001y", "too long ago"},
		{denmaCampaigns, "started>8784001h", "too long ago"},
		{denmaSubscribers, strings.Repeat("NOT ", 51) + "x", "nests more than 50"},
		{denmaLists, strings.Repeat("(", 51) + "x" + strings.Repeat(")", 51), "nests more than 50"},
		{denmaSubscribers, "bounced:maybe", "bounced is any, hard"},
		{denmaSubscribers, "confirmed:none", "isn't a filter"},
		{denmaSubscribers, "list:nope", "there's no list called"},
		{denmaSubscribers, "list:news", "matches several lists"},
		{denmaSubscribers, "opened:spring", "matches several campaigns"},
		{denmaSubscribers, "tag:,", "needs one tag"},
		{denmaLists, "campaign:spring", "matches several campaigns"},
		{denmaLists, "type:temp", "type is public"},
		{denmaLists, "subscribers>many", "is a number"},
		{denmaCampaigns, "archive:maybe", "archive is yes or no"},
		{denmaCampaigns, "template:nope", "there's no template called"},
		{denmaCampaigns, "list:nope", "there's no list called"},
		{denmaCampaigns, "format:pdf", "format is richtext"},
	} {
		sql, err := denmaTestCompile(c.kind, c.search)
		if c.want == "" { // "a AND" is fine: AND before nothing is dropped
			if err != nil {
				t.Errorf("%s %q: %v", c.kind, c.search, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s %q: no error, compiled to %s", c.kind, c.search, sql)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: %q, want it to say %q", c.kind, c.search, err, c.want)
		}
	}
}

// denmaSearchSQL itself: a mistake is a 400 that starts "Search:", and an
// empty search is no condition.
func TestDenmaSearchSQL(t *testing.T) {
	a := &App{}
	for _, s := range []string{"", "   ", `""`, `" "`} {
		if cond, err := a.denmaSearchSQL(denmaSubscribers, auth.User{}, s); cond != "" || err != nil {
			t.Errorf("%q: %q, %v", s, cond, err)
		}
	}
	_, err := a.denmaSearchSQL(denmaCampaigns, auth.User{}, "status:sent")
	var he *echo.HTTPError
	if !errors.As(err, &he) || he.Code != http.StatusBadRequest || !strings.HasPrefix(he.Message.(string), "Search: ") {
		t.Errorf("status:sent: %v", err)
	}
	for _, s := range []string{"name:a\x00b", "a\xffb"} {
		if _, err := a.denmaSearchSQL(denmaLists, auth.User{}, s); err == nil || !strings.Contains(err.Error(), "aren't text") {
			t.Errorf("%q: %v", s, err)
		}
	}
	// At the limits.
	for _, c := range []struct {
		kind   denmaKind
		search string
	}{
		{denmaSubscribers, strings.Repeat("NOT ", 50) + "x"},
		{denmaSubscribers, "created>1000y"},
		{denmaCampaigns, "started>365000d"},
	} {
		if _, err := denmaTestCompile(c.kind, c.search); err != nil {
			t.Errorf("%s %q: %v", c.kind, c.search, err)
		}
	}
	q := "x"
	if err := a.denmaSearch(auth.User{}, new(string), &q); err == nil || !strings.Contains(err.Error(), "SQL queries are turned off") {
		t.Errorf("a raw query: %v", err)
	}
	for _, s := range []string{"name:ada", "ada", "status:blocked id>=5", "created>30d attr.age>20"} {
		want, _ := denmaTestCompile(denmaSubscribers, s)
		if got, err := a.denmaSearchSQL(denmaSubscribers, auth.User{}, s); got != want || err != nil {
			t.Errorf("%q: %q, %v; want %q", s, got, err, want)
		}
	}
}

// A handful of searches and what they compile to.
func TestDenmaSearchCompiles(t *testing.T) {
	const (
		text = `(subscribers.attribs #>> ARRAY['age']::TEXT[])`
		sub  = `EXISTS (SELECT 1 FROM subscriber_lists dsl WHERE dsl.subscriber_id = subscribers.id AND dsl.status <> 'unsubscribed'`
	)
	for _, c := range []struct {
		kind         denmaKind
		search, want string
	}{
		{denmaSubscribers, `name:ada`, `((subscribers.name ILIKE '%ada%'))`},
		{denmaSubscribers, `Ada`, `(((subscribers.name ILIKE '%Ada%' OR subscribers.email ILIKE '%Ada%')))`},
		{denmaSubscribers, `name:"O'Brien"`, `((subscribers.name ILIKE '%O''Brien%'))`},
		{denmaSubscribers, `name:"50%_off"`, `((subscribers.name ILIKE  E'%50\\%\\_off%'))`},
		{denmaSubscribers, `email="A@B.org"`, `((LOWER(subscribers.email) = LOWER('A@B.org')))`},
		{denmaSubscribers, `domain:yahoo.*`, `((subscribers.email ILIKE '%@yahoo.%'))`},
		{denmaSubscribers, `status:blocked`, `((subscribers.status = 'blocklisted'))`},
		{denmaSubscribers, `-status:enabled`, `((NOT COALESCE(subscribers.status = 'enabled', FALSE)))`},
		{denmaSubscribers, `id>=5`, `((subscribers.id >= 5))`},
		{denmaSubscribers, `name:a OR name:b`, `(((subscribers.name ILIKE '%a%') OR (subscribers.name ILIKE '%b%')))`},
		{denmaSubscribers, `name:a AND (id:1 OR id:2)`, `((subscribers.name ILIKE '%a%' AND ((subscribers.id = 1) OR (subscribers.id = 2))))`},
		{denmaSubscribers, `list:Newsletter`, `((` + sub + ` AND dsl.list_id = 1)))`},
		{denmaSubscribers, `list:digest`, `((` + sub + ` AND dsl.list_id = 2)))`},
		{denmaSubscribers, `list:3`, `((` + sub + ` AND dsl.list_id = 3)))`},
		{denmaSubscribers, `list:none`, `((NOT ` + sub + `)))`},
		{denmaSubscribers, `created:2024-01-31`, `((subscribers.created_at >= '2024-01-31'::DATE AND subscribers.created_at < ('2024-01-31'::DATE + 1)))`},
		{denmaSubscribers, `created>30d`, `((subscribers.created_at > (NOW() - INTERVAL '30 days')))`},
		{denmaSubscribers, `attr.age>20`, `(((CASE WHEN TRIM(` + text + `) ~ '^-?[0-9]+([.][0-9]+)?$' THEN TRIM(` + text + `)::NUMERIC END) > 20))`},
		{denmaSubscribers, `attr.age>x`, `((` + text + ` > 'x'))`},
		{denmaSubscribers, `has:attr.address.city`, `((COALESCE((subscribers.attribs #>> ARRAY['address', 'city']::TEXT[]), '') NOT IN ('', 'null', '[]', '{}')))`},
		{denmaSubscribers, `tag:Volunteer`, `(((jsonb_typeof(subscribers.attribs->'tags') = 'array' AND subscribers.attribs->'tags' ? 'volunteer')))`},
		{denmaSubscribers, `opened:autumn`, `((EXISTS (SELECT 1 FROM campaign_views dact WHERE dact.subscriber_id = subscribers.id AND dact.campaign_id = 9)))`},
		{denmaSubscribers, `bounced:HARD`, `((EXISTS (SELECT 1 FROM bounces dact WHERE dact.subscriber_id = subscribers.id AND dact.type = 'hard')))`},
		{denmaLists, `type:public`, `((l.type = 'public'))`},
		{denmaLists, `Newsletter`, `((l.name ILIKE '%Newsletter%'))`},
		{denmaLists, `subscribers>10`, `(((SELECT COUNT(*) FROM subscriber_lists dsl WHERE dsl.list_id = l.id AND dsl.status <> 'unsubscribed') > 10))`},
		{denmaCampaigns, `archive:yes`, `((c.archive))`},
		{denmaCampaigns, `template:default`, `((c.template_id = 4))`},
		{denmaCampaigns, `"Last call: Autumn"`, `(((c.name ILIKE '%Last call: Autumn%' OR c.subject ILIKE '%Last call: Autumn%')))`},
		{denmaCampaigns, `call: x`, `(((c.name ILIKE '%call:%' OR c.subject ILIKE '%call:%') AND (c.name ILIKE '%x%' OR c.subject ILIKE '%x%')))`},
	} {
		got, err := denmaTestCompile(c.kind, c.search)
		if err != nil || got != c.want {
			t.Errorf("%s %s:\n got %s (%v)\nwant %s", c.kind, c.search, got, err, c.want)
		}
	}
}

// denmaSplitSQL itself, so the checks above mean what they say.
func TestDenmaSearchSplitSQL(t *testing.T) {
	for _, c := range []struct {
		in, code string
		lits     []string
		ok       bool
	}{
		{`a = 'x'`, `a = ?`, []string{"x"}, true},
		{`a = 'it''s' AND b`, `a = ? AND b`, []string{"it's"}, true},
		{`a ILIKE  E'\\%x\''`, `a ILIKE  ?`, []string{`\%x'`}, true},
		{`a = 'x\' OR 1=1`, `a = ? OR 1=1`, []string{`x\`}, true},
		{`a = 'open`, `a = `, nil, false},
		{`TRUE'x'`, `TRUE?`, []string{"x"}, true},
	} {
		code, lits, ok := denmaSplitSQL(c.in)
		if code != c.code || ok != c.ok || strings.Join(lits, "|") != strings.Join(c.lits, "|") {
			t.Errorf("%s: %q %q %v", c.in, code, lits, ok)
		}
	}
}
