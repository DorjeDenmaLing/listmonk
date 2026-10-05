package main

// denma: people, and the centers they're users of. A person signs in once, at
// /login (cmd/denma_login.go), with their e-mail address or username, their
// password and, if they've turned it on, a two-factor code, and can be a user
// of several centers. What signs them in is theirs, kept with the centers'
// registry (denma.people). What they can do in a center is that center's:
// their account there is an ordinary listmonk user with its own username,
// role, list role and status, which the center's admins manage.
// denma.memberships links the two, and only the hub reads it, so a center
// doesn't learn of a person's other centers, their roles there, or anyone
// else's users.
//
// A person's account in a center has password login but no password of its
// own (listmonk's own sign-in can't let them in there), and the center's
// admins can't change its e-mail address or password. Those are the person's,
// changed on their Profile page in any of their centers, for all of them, as
// is two-factor sign-in.
//
// New users are added by e-mail address (addMember). Someone new is e-mailed a
// link to set their password; someone who already has an account is told
// they can now sign in to this center with it. Either way the admin sees the
// same, so adding someone doesn't tell them whether that person is another
// center's user. Deleting a person's account in their last center deletes
// the person.
//
// When a center loads, its users who aren't people yet become people with
// their own password and two-factor key (adoptPeople). Accounts in different
// centers with the same e-mail address become one person, who keeps the
// password of the account adopted first.
//
// The hub's users, the superadmins, aren't people. They sign in to the hub
// with listmonk's sign-in and open centers from there (cmd/denma_hub.go). A
// username is a superadmin's or a person's, never both.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"github.com/pquerna/otp/totp"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	// How long a new person's set-password link works.
	denmaInviteTTL = 7 * 24 * time.Hour

	// A person's columns, as denmaPerson.
	denmaPersonCols = `p.id, p.username, p.email, p.name, p.password IS NOT NULL AS has_password, p.twofa_key, p.loggedin_at`
)

var (
	errDenmaUsernameTaken = echo.NewHTTPError(http.StatusBadRequest, "Username already taken")
	errDenmaEmailTaken    = echo.NewHTTPError(http.StatusBadRequest, "E-mail address already taken")
)

// denmaInitPeople creates the people and their memberships (with the registry).
func denmaInitPeople(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS denma.people (
			id          SERIAL PRIMARY KEY,
			username    TEXT NOT NULL,
			email       TEXT NOT NULL, -- lowercase
			name        TEXT NOT NULL,
			password    TEXT NULL,     -- bcrypt; none until they set one
			twofa_key   TEXT NULL,     -- their TOTP secret, with two-factor sign-in on
			created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			loggedin_at TIMESTAMP WITH TIME ZONE NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS people_username ON denma.people (LOWER(username));
		CREATE UNIQUE INDEX IF NOT EXISTS people_email ON denma.people (LOWER(email));
		-- A person's account in each of their centers.
		CREATE TABLE IF NOT EXISTS denma.memberships (
			person_id      INTEGER NOT NULL REFERENCES denma.people(id) ON DELETE CASCADE,
			center_id      INTEGER NOT NULL REFERENCES denma.centers(id) ON DELETE CASCADE,
			center_user_id INTEGER NOT NULL,
			PRIMARY KEY (center_id, center_user_id),
			UNIQUE (person_id, center_id)
		);
		-- A person's set-password link (a new person's, or a forgotten
		-- password's): its SHA-256, so the database can't sign anyone in.
		CREATE TABLE IF NOT EXISTS denma.person_tokens (
			person_id  INTEGER PRIMARY KEY REFERENCES denma.people(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL,
			expires_at TIMESTAMP WITH TIME ZONE NOT NULL
		);
		-- Center admins' set-password links, from before people.
		DROP TABLE IF EXISTS denma.invites;`)
	return err
}

// denmaPerson is someone who signs in at /login.
type denmaPerson struct {
	ID          int         `db:"id"`
	Username    string      `db:"username"`
	Email       string      `db:"email"`
	Name        string      `db:"name"`
	HasPassword bool        `db:"has_password"`
	TwofaKey    null.String `db:"twofa_key"`
	LoggedInAt  null.Time   `db:"loggedin_at"`
}

// denmaMembership is a person's account in a center.
type denmaMembership struct {
	PersonID int    `db:"person_id"`
	CenterID int    `db:"center_id"`
	UserID   int    `db:"center_user_id"`
	Slug     string `db:"slug"`
	Name     string `db:"name"` // the center's
	Schema   string `db:"schema_name"`
}

func denmaTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func denmaUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

// db is the database the registry is in (every center's).
func (d *denmaCenters) db() *sqlx.DB {
	return d.current().db
}

// person returns a person by ID, or nil.
func (d *denmaCenters) person(id int) (*denmaPerson, error) {
	var p denmaPerson
	err := d.db().Get(&p, `SELECT `+denmaPersonCols+` FROM denma.people p WHERE p.id = $1`, id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// personByEmail returns the person with an e-mail address, or nil.
func (d *denmaCenters) personByEmail(q sqlx.Queryer, email string) (*denmaPerson, error) {
	var p denmaPerson
	err := sqlx.Get(q, &p, `SELECT `+denmaPersonCols+` FROM denma.people p WHERE LOWER(p.email) = LOWER($1)`, email)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// memberships returns a person's accounts, in centers that are enabled, by
// center name.
func (d *denmaCenters) memberships(personID int) ([]denmaMembership, error) {
	var out []denmaMembership
	err := d.db().Select(&out, `SELECT m.person_id, m.center_id, m.center_user_id, c.slug, c.name, c.schema_name
		FROM denma.memberships m JOIN denma.centers c ON c.id = m.center_id
		WHERE m.person_id = $1 AND c.status = 'enabled' ORDER BY LOWER(c.name)`, personID)
	return out, err
}

// allMemberships is memberships in every center, enabled or not, for
// changes that must reach all of a person's accounts.
func (d *denmaCenters) allMemberships(q sqlx.Queryer, personID int) ([]denmaMembership, error) {
	var out []denmaMembership
	err := sqlx.Select(q, &out, `SELECT m.person_id, m.center_id, m.center_user_id, c.slug, c.name, c.schema_name
		FROM denma.memberships m JOIN denma.centers c ON c.id = m.center_id WHERE m.person_id = $1`, personID)
	return out, err
}

// centersOf returns the person's accounts that they can sign in to now: in
// running centers, and enabled there.
func (d *denmaCenters) centersOf(personID int) ([]denmaMembership, error) {
	ms, err := d.memberships(personID)
	if err != nil {
		return nil, err
	}
	out := ms[:0]
	for _, m := range ms {
		ctr := d.get(m.Slug)
		if ctr == nil {
			continue
		}
		var status string
		if err := ctr.app.db.Get(&status, `SELECT status FROM users WHERE id = $1 AND type = 'user'`, m.UserID); err != nil {
			if err != sql.ErrNoRows {
				lo.Printf("denma: error reading the account of person %d in center %s: %v", personID, m.Slug, err)
			}
			continue
		}
		if status == string(auth.UserStatusEnabled) {
			out = append(out, m)
		}
	}
	return out, nil
}

// usernameTaken reports whether a username is a person's (but personID's)
// or a superadmin's.
func (d *denmaCenters) usernameTaken(q sqlx.Queryer, username string, personID int) (bool, error) {
	var taken bool
	err := sqlx.Get(q, &taken, `SELECT EXISTS (SELECT 1 FROM denma.people WHERE LOWER(username) = LOWER($1) AND id <> $2)
		OR EXISTS (SELECT 1 FROM `+pq.QuoteIdentifier(d.baseSchema)+`.users WHERE LOWER(username) = LOWER($1) AND type = 'user')`,
		username, personID)
	return taken, err
}

// eachAccount runs a statement on every one of a person's accounts, in their
// centers' schemas: q's S. is the schema, $1 the account's ID, and args the
// rest.
func (d *denmaCenters) eachAccount(tx *sqlx.Tx, personID int, q string, args ...any) error {
	ms, err := d.allMemberships(tx, personID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if _, err := tx.Exec(denmaInSchema(q, m.Schema), append([]any{m.UserID}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// endSessions signs a person out of all their centers, but for the session
// keep (in any of them).
func (d *denmaCenters) endSessions(tx *sqlx.Tx, personID int, keep string) error {
	return d.eachAccount(tx, personID, `DELETE FROM S.sessions WHERE data->>'user_id' = $1::TEXT AND id <> $2`, keep)
}

// setPassword sets a person's password, forgets their set-password link and
// signs them out everywhere but the session keep.
func (d *denmaCenters) setPassword(personID int, password, keep string) error {
	tx, err := d.db().Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE denma.people SET password = crypt($2, gen_salt('bf')), updated_at = NOW() WHERE id = $1`, personID, password); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM denma.person_tokens WHERE person_id = $1`, personID); err != nil {
		return err
	}
	if err := d.endSessions(tx, personID, keep); err != nil {
		return err
	}
	return tx.Commit()
}

// setTwofa turns a person's two-factor sign-in on (with key) or off (""),
// and shows it in every one of their accounts.
func (d *denmaCenters) setTwofa(personID int, key string) error {
	tx, err := d.db().Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	typ := models.TwofaTypeNone
	if key != "" {
		typ = models.TwofaTypeTOTP
	}
	if _, err := tx.Exec(`UPDATE denma.people SET twofa_key = NULLIF($2, ''), updated_at = NOW() WHERE id = $1`, personID, key); err != nil {
		return err
	}
	if err := d.eachAccount(tx, personID, `UPDATE S.users SET twofa_type = $2::S.twofa_type, twofa_key = NULL, updated_at = NOW() WHERE id = $1`, typ); err != nil {
		return err
	}
	return tx.Commit()
}

// newToken makes a person's set-password link token, replacing any earlier
// one.
func (d *denmaCenters) newToken(personID int, ttl time.Duration) (string, error) {
	token, err := generateRandomString(tmpAuthTokenLen)
	if err != nil {
		return "", err
	}
	_, err = d.db().Exec(`WITH gone AS (DELETE FROM denma.person_tokens WHERE expires_at < NOW())
		INSERT INTO denma.person_tokens (person_id, token_hash, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (person_id) DO UPDATE SET token_hash = EXCLUDED.token_hash, expires_at = EXCLUDED.expires_at`,
		personID, denmaTokenHash(token), time.Now().Add(ttl))
	return token, err
}

// tokenPerson returns the person a set-password link is for, or nil if it's
// not (or no longer) good.
func (d *denmaCenters) tokenPerson(email, token string) (*denmaPerson, error) {
	if email == "" || len(token) < tmpAuthTokenLen {
		return nil, nil
	}
	var p denmaPerson
	err := d.db().Get(&p, `SELECT `+denmaPersonCols+` FROM denma.person_tokens t JOIN denma.people p ON p.id = t.person_id
		WHERE LOWER(p.email) = LOWER($1) AND t.token_hash = $2 AND t.expires_at > NOW()`, email, denmaTokenHash(token))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// resetURL is a person's set-password link.
func (d *denmaCenters) resetURL(p *denmaPerson, token string) string {
	return fmt.Sprintf("%s%s?token=%s&email=%s", strings.TrimSuffix(d.current().urlCfg.RootURL, "/"), denmaResetPath,
		url.QueryEscape(token), url.QueryEscape(p.Email))
}

// loginURL is where everyone signs in.
func (d *denmaCenters) loginURL() string {
	return strings.TrimSuffix(d.current().urlCfg.RootURL, "/") + denmaLoginPath
}

// adoptPeople makes a center's users people, on every load (adopt): each
// one that isn't one yet, but the superadmins' accounts. Their password and
// two-factor key move to the person; an account with the e-mail address of
// someone who's a person already becomes theirs.
func (d *denmaCenters) adoptPeople(c *denmaCenter, db *sqlx.DB) error {
	var users []struct {
		ID        int         `db:"id"`
		Username  string      `db:"username"`
		Email     string      `db:"email"`
		Name      string      `db:"name"`
		Password  null.String `db:"password"`
		TwofaType string      `db:"twofa_type"`
		TwofaKey  null.String `db:"twofa_key"`
	}
	if err := db.Select(&users, `SELECT id, username, COALESCE(email, '') AS email, name, password, twofa_type::TEXT AS twofa_type, twofa_key
		FROM users WHERE type = 'user'
		AND id NOT IN (SELECT center_user_id FROM denma.center_superadmins WHERE center_id = $1)
		AND id NOT IN (SELECT center_user_id FROM denma.memberships WHERE center_id = $1)
		ORDER BY id`, c.ID); err != nil {
		return fmt.Errorf("reading the users who aren't people yet: %v", err)
	}

	for _, u := range users {
		if u.Email == "" {
			lo.Printf("denma: center %s: user %s has no e-mail address, so can't sign in at /login; give them one", c.Slug, u.Username)
			continue
		}
		key := null.String{}
		if u.TwofaType == models.TwofaTypeTOTP {
			key = u.TwofaKey
		}
		err := func() error {
			tx, err := db.Beginx()
			if err != nil {
				return err
			}
			defer tx.Rollback()

			p, err := d.personByEmail(tx, u.Email)
			if err != nil {
				return err
			}
			pid := 0
			if p != nil {
				pid = p.ID
				lo.Printf("denma: center %s: user %s is %s, who signs in with their own password", c.Slug, u.Username, p.Email)
			} else {
				uname := u.Username
				for i := 1; ; i++ {
					taken, err := d.usernameTaken(tx, uname, 0)
					if err != nil {
						return err
					}
					if !taken {
						break
					}
					uname = fmt.Sprintf("%s-%s", u.Username, c.Slug)
					if i > 1 {
						uname = fmt.Sprintf("%s-%s-%d", u.Username, c.Slug, i)
					}
				}
				if uname != u.Username {
					lo.Printf("denma: center %s: user %s now signs in as %s (or with their e-mail address): the username is someone else's", c.Slug, u.Username, uname)
				}
				if err := tx.Get(&pid, `INSERT INTO denma.people (username, email, name, password, twofa_key) VALUES ($1, LOWER($2), $3, $4, $5) RETURNING id`,
					uname, u.Email, u.Name, u.Password, key); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`INSERT INTO denma.memberships (person_id, center_id, center_user_id) VALUES ($1, $2, $3)`, pid, c.ID, u.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE users SET password = NULL, password_login = TRUE, twofa_key = NULL,
				twofa_type = (SELECT CASE WHEN twofa_key IS NULL THEN 'none' ELSE 'totp' END FROM denma.people WHERE id = $2)::twofa_type
				WHERE id = $1`, u.ID, pid); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return fmt.Errorf("making user %s a person: %v", u.Username, err)
		}
	}
	return nil
}

// denmaInvite is what addMember did to tell the person.
type denmaInvite struct {
	URL  string // a new person's set-password link
	Sent bool   // the e-mail went
}

// addMember makes the person with u's e-mail address (a new one, with u's
// username and name, if there's none) a user of ctr, as u (its username,
// name, role, list role and status), and e-mails them: a new person (or one
// who hasn't set a password yet) a link to set their password, anyone else
// that they can now sign in to this center.
func (d *denmaCenters) addMember(ctr *denmaCenter, u auth.User) (auth.User, denmaInvite, error) {
	db := d.db()
	email := strings.ToLower(strings.TrimSpace(u.Email.String))

	p, err := d.personByEmail(db, email)
	if err != nil {
		return auth.User{}, denmaInvite{}, err
	}
	isNew := p == nil
	if isNew {
		taken, err := d.usernameTaken(db, u.Username, 0)
		if err != nil {
			return auth.User{}, denmaInvite{}, err
		}
		if taken {
			return auth.User{}, denmaInvite{}, errDenmaUsernameTaken
		}
		p = &denmaPerson{}
		if err := db.Get(p, `INSERT INTO denma.people AS p (username, email, name) VALUES ($1, $2, $3) RETURNING `+denmaPersonCols,
			u.Username, email, u.Name); err != nil {
			if denmaUniqueViolation(err) {
				return auth.User{}, denmaInvite{}, echo.NewHTTPError(http.StatusConflict, "Someone else just added this person. Try again.")
			}
			return auth.User{}, denmaInvite{}, err
		}
	}
	undo := func() {
		if isNew {
			_, _ = db.Exec(`DELETE FROM denma.people WHERE id = $1`, p.ID)
		}
	}

	// Their account in the center, without a password of its own.
	u.Type = auth.UserTypeUser
	u.PasswordLogin = true
	u.Password = null.String{}
	u.Email = null.StringFrom(email)
	user, err := ctr.app.core.CreateUser(u)
	if err != nil {
		undo()
		return auth.User{}, denmaInvite{}, err
	}
	if _, err := db.Exec(`INSERT INTO denma.memberships (person_id, center_id, center_user_id) VALUES ($1, $2, $3)`, p.ID, ctr.ID, user.ID); err != nil {
		_, _ = ctr.app.db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
		undo()
		return auth.User{}, denmaInvite{}, err
	}
	if p.TwofaKey.Valid {
		if _, err := ctr.app.db.Exec(`UPDATE users SET twofa_type = 'totp' WHERE id = $1`, user.ID); err != nil {
			lo.Printf("denma: error showing two-factor sign-in on %s's account in %s: %v", p.Username, ctr.Slug, err)
		}
		user.TwofaType = models.TwofaTypeTOTP
	}
	user.Password = null.String{}

	inv := denmaInvite{}
	if !p.HasPassword {
		token, err := d.newToken(p.ID, denmaInviteTTL)
		if err != nil {
			lo.Printf("denma: error making the set-password link for %s: %v", p.Email, err)
			return user, inv, nil
		}
		inv.URL = d.resetURL(p, token)
	}
	inv.Sent = d.sendInvite(ctr, p, inv.URL)
	return user, inv, nil
}

// sendInvite e-mails a person who's been made a user of ctr, in its
// notification look (header and footer from its e-mail templates, which
// --static-dir may replace wholesale, so the invite itself is defined here,
// added to them when the center loads: denmaAddInviteTpl): with link, to set
// their password; without, that they can sign in.
func (d *denmaCenters) sendInvite(ctr *denmaCenter, p *denmaPerson, link string) bool {
	app := ctr.app
	site := app.ko.String("app.site_name")
	var body bytes.Buffer
	err := app.notifs.Tpls.ExecuteTemplate(&body, "denma-invite", map[string]any{
		"ResetURL": link,
		"LoginURL": d.loginURL(),
		"Site":     site,
		"Email":    p.Email,
		"Username": p.Username,
		"Days":     int(denmaInviteTTL.Hours() / 24),
	})
	if err != nil {
		lo.Printf("denma: error rendering the invite for %s: %v", p.Email, err)
		return false
	}
	subject := fmt.Sprintf("You can now manage %s's mailing lists", site)
	if link != "" {
		subject = fmt.Sprintf("Your %s account", site)
	}
	subject, b := notifs.GetTplSubject(subject, body.Bytes())
	if err := app.emailMsgr.Push(models.Message{
		From:    app.cfg.FromEmail,
		To:      []string{p.Email},
		Subject: subject,
		Body:    b,
	}); err != nil {
		lo.Printf("denma: error sending the invite to %s: %v", p.Email, err)
		return false
	}
	return true
}

// denmaAddInviteTpl adds the invite to a center's e-mail templates, before
// they're first used (html/template can't add to, or copy, a set that has
// been executed).
func denmaAddInviteTpl(tpls *template.Template) error {
	_, err := tpls.New("denma-invite").Parse(denmaInviteTpl)
	return err
}

const denmaInviteTpl = `{{ template "header" . }}
{{- if .ResetURL }}
<h2>Your {{ .Site }} account</h2>
<p>An account has been made for you to manage {{ .Site }}'s mailing lists. You sign in at <a href="{{ .LoginURL }}">{{ .LoginURL }}</a> with your e-mail address, {{ .Email }}, or your username, <strong>{{ .Username }}</strong>.</p>
<p><a href="{{ .ResetURL }}" class="button">Set your password</a></p>
<p style="color: #666; font-size: 12px;">This link works for {{ .Days }} days. After that, use "Forgot password" on the sign-in page.</p>
{{- else }}
<h2>You can now manage {{ .Site }}'s mailing lists</h2>
<p>Sign in at <a href="{{ .LoginURL }}">{{ .LoginURL }}</a> with your e-mail address, {{ .Email }}, and your password, as you do already. If you're a user of more than one center, you choose {{ .Site }} after signing in, or switch to it from the menu under your picture at the top right.</p>
<p><a href="{{ .LoginURL }}" class="button">Sign in</a></p>
{{- end }}
{{ template "footer" }}`

// denmaCenterOf is the center this app is, or nil (the hub, or one install).
func (a *App) denmaCenterOf() *denmaCenter {
	if !a.denmaInCenter() {
		return nil
	}
	return denmaHub.get(a.ko.String("denma.center"))
}

// denmaMemberOf returns the person whose account in this center the user
// with this ID is, or nil (an API user, a superadmin's account, the hub, one
// install).
func (a *App) denmaMemberOf(userID int) (*denmaPerson, error) {
	if !a.denmaInCenter() {
		return nil, nil
	}
	var p denmaPerson
	err := a.db.Get(&p, `SELECT `+denmaPersonCols+` FROM denma.memberships m JOIN denma.centers c ON c.id = m.center_id
		JOIN denma.people p ON p.id = m.person_id WHERE c.slug = $1 AND m.center_user_id = $2`, a.ko.String("denma.center"), userID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return &p, nil
}

// denmaIsMember reports whether a user is a person's account (or might be:
// on an error, yes), for listmonk's own password reset, which isn't theirs
// (cmd/auth.go).
func (a *App) denmaIsMember(userID int) bool {
	p, err := a.denmaMemberOf(userID)
	return p != nil || err != nil
}

// denmaCheckUserUnique refuses a hub user's username that's a person's
// (/login tells them apart by username). Centers' usernames are their own.
func (a *App) denmaCheckUserUnique(u auth.User) error {
	if denmaHub == nil || a.denmaInCenter() || u.Type == auth.UserTypeAPI {
		return nil
	}
	var taken bool
	if err := a.db.Get(&taken, `SELECT EXISTS (SELECT 1 FROM denma.people WHERE LOWER(username) = LOWER($1))`, u.Username); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if taken {
		return errDenmaUsernameTaken
	}
	return nil
}

// denmaCreateMember adds a user in a center (listmonk's CreateUser, once
// it's checked the form): a person, by e-mail address (addMember). API users
// are listmonk's.
func (a *App) denmaCreateMember(c echo.Context, u auth.User) (bool, error) {
	ctr := a.denmaCenterOf()
	if ctr == nil || u.Type == auth.UserTypeAPI {
		return false, nil
	}
	user, _, err := denmaHub.addMember(ctr, u)
	if err != nil {
		return true, err
	}
	return true, c.JSON(http.StatusOK, okResp{user})
}

// denmaUpdateMember saves a person's account in a center (listmonk's
// UpdateUser, once it's checked the form and that the editor may): its
// username, name, roles and status. Their e-mail address and password stay
// theirs. Other users (API users) are listmonk's, and stay API users.
func (a *App) denmaUpdateMember(c echo.Context, id int, u auth.User) (bool, error) {
	if !a.denmaInCenter() {
		return false, nil
	}
	p, err := a.denmaMemberOf(id)
	if err != nil {
		return true, err
	}
	if p == nil {
		if u.Type != auth.UserTypeAPI {
			return true, echo.NewHTTPError(http.StatusBadRequest, "An API user can't become a user who signs in. Add the person as a new user.")
		}
		return false, nil
	}

	if u.Name == "" {
		u.Name = u.Username
	}
	if err := a.denmaCheckAssign(c, u.UserRoleID, u.ListRoleID); err != nil {
		return true, err
	}
	user, err := a.core.UpdateUser(id, auth.User{
		Username:      u.Username,
		Name:          u.Name,
		PasswordLogin: true, // with no password: theirs is the person's
		Type:          auth.UserTypeUser,
		UserRoleID:    u.UserRoleID,
		ListRoleID:    u.ListRoleID,
		Status:        u.Status,
	})
	if err != nil {
		return true, err
	}
	user.Password = null.String{}
	if _, err := cacheUsers(a.core, a.auth); err != nil {
		return true, err
	}
	return true, c.JSON(http.StatusOK, okResp{user})
}

// denmaUsersDeleted forgets the deleted users' memberships, and deletes the
// people who were users of this center only.
func (a *App) denmaUsersDeleted(ids []int) {
	if !a.denmaInCenter() {
		return
	}
	var people []int
	if err := a.db.Select(&people, `DELETE FROM denma.memberships WHERE center_user_id = ANY($2)
		AND center_id = (SELECT id FROM denma.centers WHERE slug = $1) RETURNING person_id`,
		a.ko.String("denma.center"), pq.Array(ids)); err != nil {
		a.log.Printf("denma: error removing deleted users' memberships: %v", err)
		return
	}
	if len(people) == 0 {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM denma.people p WHERE p.id = ANY($1)
		AND NOT EXISTS (SELECT 1 FROM denma.memberships m WHERE m.person_id = p.id)`, pq.Array(people)); err != nil {
		a.log.Printf("denma: error deleting people who have no centers: %v", err)
	}
}

// denmaUpdateOwnAccount saves a person's Profile (listmonk's
// UpdateUserProfile): their name, e-mail address and password, for every one
// of their centers.
func (a *App) denmaUpdateOwnAccount(c echo.Context) (bool, error) {
	user := auth.GetUser(c)
	p, err := a.denmaMemberOf(user.ID)
	if err != nil || p == nil {
		return err != nil, err
	}

	var u auth.User
	if err := c.Bind(&u); err != nil {
		return true, err
	}
	var (
		name     = strings.TrimSpace(u.Name)
		email    = strings.ToLower(strings.TrimSpace(u.Email.String))
		password = u.Password.String
	)
	if name == "" {
		name = p.Name
	}
	if !utils.ValidateEmail(email) {
		return true, echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.invalidFields", "name", "email"))
	}
	if password != "" && !strHasLen(password, 8, stdInputMaxLen) {
		return true, echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.invalidFields", "name", "password"))
	}

	err = func() error {
		tx, err := a.db.Beginx()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE denma.people SET name = $2, email = $3, updated_at = NOW() WHERE id = $1`, p.ID, name, email); err != nil {
			return err
		}
		if err := denmaHub.eachAccount(tx, p.ID, `UPDATE S.users SET name = $2, email = $3, updated_at = NOW() WHERE id = $1`, name, email); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if denmaUniqueViolation(err) {
		return true, errDenmaEmailTaken
	}
	if err != nil {
		return true, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if password != "" {
		if err := denmaHub.setPassword(p.ID, password, auth.GetSessionID(c)); err != nil {
			return true, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	out, err := a.core.GetUser(user.ID, "", "")
	if err != nil {
		return true, err
	}
	out.Password = null.String{}
	return true, c.JSON(http.StatusOK, okResp{out})
}

// denmaEnableTOTP turns on a person's two-factor sign-in, for every one of
// their centers (listmonk's EnableTOTP, once it has the secret and code).
func (a *App) denmaEnableTOTP(c echo.Context, secret, code string) (bool, error) {
	p, err := a.denmaMemberOf(auth.GetUser(c).ID)
	if err != nil || p == nil {
		return err != nil, err
	}
	if p.TwofaKey.Valid {
		return true, echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("users.twoFAAlreadyEnabled"))
	}
	if !totp.Validate(code, secret) {
		return true, echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("users.invalidTOTPCode"))
	}
	if err := denmaHub.setTwofa(p.ID, secret); err != nil {
		return true, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return true, c.JSON(http.StatusOK, okResp{true})
}

// denmaDisableTOTP turns off a person's two-factor sign-in, with their
// password (listmonk's DisableTOTP, once it has the password).
func (a *App) denmaDisableTOTP(c echo.Context, password string) (bool, error) {
	p, err := a.denmaMemberOf(auth.GetUser(c).ID)
	if err != nil || p == nil {
		return err != nil, err
	}
	if !p.TwofaKey.Valid {
		return true, echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("users.twoFANotEnabled"))
	}
	if ok, err := denmaHub.passwordOK(p.ID, password); err != nil || !ok {
		return true, echo.NewHTTPError(http.StatusForbidden, a.i18n.T("users.invalidPassword"))
	}
	if err := denmaHub.setTwofa(p.ID, ""); err != nil {
		return true, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return true, c.JSON(http.StatusOK, okResp{true})
}

// passwordOK reports whether password is the person's.
func (d *denmaCenters) passwordOK(personID int, password string) (bool, error) {
	var ok bool
	err := d.db().Get(&ok, `SELECT EXISTS (SELECT 1 FROM denma.people WHERE id = $1 AND password IS NOT NULL AND crypt($2, password) = password)`,
		personID, password)
	return ok, err
}

// denmaTOTPIssuer is the name authenticator apps show for the code: for a
// person, the hub's, as the code is for all their centers.
func (a *App) denmaTOTPIssuer() string {
	if a.denmaInCenter() {
		return denmaHub.current().cfg.SiteName
	}
	return a.cfg.SiteName
}

// initDenmaPeopleHandlers registers switching centers (in a center) and the
// hub's People page and its API.
func initDenmaPeopleHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/switch/:slug"), a.DenmaSwitchCenter)
	g.GET(path.Join(uriAdmin, "/people"), a.ViewDenmaPeople)
}

func initDenmaPeopleAPIHandlers(g *echo.Group, a *App) {
	g.DELETE("/api/denma/people/:id/twofa", hasID(a.DenmaPersonTwofaOff))
}

// DenmaSwitchCenter signs a person into another of their centers, from the
// menu under their picture (partials/denma/topnav.html).
func (a *App) DenmaSwitchCenter(c echo.Context) error {
	p, err := a.denmaMemberOf(auth.GetUser(c).ID)
	if err != nil {
		return err
	}
	if p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	ms, err := denmaHub.centersOf(p.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for _, m := range ms {
		if m.Slug == c.Param("slug") {
			ctr, err := denmaHub.enter(c, m)
			if err != nil {
				return err
			}
			// In full: a center's own redirects get its prefix (denmaPrefixWriter).
			return c.Redirect(http.StatusFound, strings.TrimSuffix(ctr.app.urlCfg.RootURL, "/")+uriAdmin)
		}
	}
	return echo.NewHTTPError(http.StatusNotFound, "You're not a user of that center, or your account there is disabled.")
}

// enter signs someone into a center as their account there, and records it
// in the center's activity log. It doesn't say where they came from (another
// of their centers): that's not the center's to know.
func (d *denmaCenters) enter(c echo.Context, m denmaMembership) (*denmaCenter, error) {
	ctr := d.get(m.Slug)
	if ctr == nil {
		return nil, echo.NewHTTPError(http.StatusServiceUnavailable, "That center isn't running. Try again in a moment.")
	}
	user, err := ctr.app.core.GetUser(m.UserID, "", "")
	if err != nil {
		return nil, err
	}
	if user.Status != auth.UserStatusEnabled {
		return nil, echo.NewHTTPError(http.StatusForbidden, "Your account in that center is disabled.")
	}
	// The center's session cookie (its auth sets the center's cookie path).
	if err := ctr.app.auth.SaveSession(user, "", c); err != nil {
		return nil, err
	}
	if err := ctr.app.core.UpdateUserLogin(user.ID, ""); err != nil {
		lo.Printf("denma: error recording %s's sign-in in center %s: %v", user.Username, ctr.Slug, err)
	}
	if _, err := ctr.app.db.Exec(`INSERT INTO denma.audit_log (center, user_id, username, user_name, ip, method, path, route, action, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Signed in', $9)`,
		ctr.Slug, user.ID, user.Username, user.Name, c.RealIP(), c.Request().Method, denmaLoginPath, denmaLoginPath, http.StatusFound); err != nil {
		lo.Printf("denma: error recording a sign-in in center %s's activity log: %v", ctr.Slug, err)
	}
	return ctr, nil
}

// denmaOtherCenter is a center in a person's switcher.
type denmaOtherCenter struct {
	Slug string `db:"slug" json:"slug"`
	Name string `db:"name" json:"name"`
}

// denmaOtherCenters lists, for the menu under a person's picture, their
// other centers than slug's, where userID is their account.
func denmaOtherCenters(slug string, userID int) []denmaOtherCenter {
	if denmaHub == nil || slug == "" {
		return nil
	}
	var out []denmaOtherCenter
	if err := denmaHub.db().Select(&out, `SELECT c.slug, c.name FROM denma.memberships me
		JOIN denma.centers mc ON mc.id = me.center_id
		JOIN denma.memberships m ON m.person_id = me.person_id AND m.center_id <> me.center_id
		JOIN denma.centers c ON c.id = m.center_id AND c.status = 'enabled'
		WHERE mc.slug = $1 AND me.center_user_id = $2 ORDER BY LOWER(c.name)`, slug, userID); err != nil {
		lo.Printf("denma: error listing a person's centers: %v", err)
		return nil
	}
	return out
}

// denmaPersonRow is a person on the hub's People page, with their accounts.
type denmaPersonRow struct {
	denmaPerson
	Centers []denmaPersonCenter
}

type denmaPersonCenter struct {
	Slug     string `db:"slug"`
	Name     string `db:"name"`
	UserID   int    `db:"id"`
	Username string `db:"username"`
	Role     string `db:"role"`
	Status   string `db:"status"`
}

type denmaPeopleView struct {
	adminView
	People []denmaPersonRow
}

// ViewDenmaPeople renders the hub's People page (views/denma-people.html):
// everyone who signs in at /login, their centers and their role in each.
func (a *App) ViewDenmaPeople(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	var people []denmaPerson
	if err := d.db().Select(&people, `SELECT `+denmaPersonCols+` FROM denma.people p ORDER BY LOWER(p.name), p.id`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// Their accounts, from every center that has people: one query.
	var centers []struct {
		ID     int    `db:"id"`
		Slug   string `db:"slug"`
		Name   string `db:"name"`
		Schema string `db:"schema_name"`
	}
	if err := d.db().Select(&centers, `SELECT c.id, c.slug, c.name, c.schema_name FROM denma.centers c
		WHERE EXISTS (SELECT 1 FROM denma.memberships m WHERE m.center_id = c.id)`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	byPerson := map[int][]denmaPersonCenter{}
	if len(centers) > 0 {
		parts := make([]string, len(centers))
		for i, ce := range centers {
			parts[i] = fmt.Sprintf(`SELECT m.person_id, %s AS slug, %s AS name, u.id, u.username, COALESCE(r.name, '') AS role, u.status::TEXT AS status
				FROM denma.memberships m JOIN %[3]s.users u ON u.id = m.center_user_id LEFT JOIN %[3]s.roles r ON r.id = u.user_role_id
				WHERE m.center_id = %d`, pq.QuoteLiteral(ce.Slug), pq.QuoteLiteral(ce.Name), pq.QuoteIdentifier(ce.Schema), ce.ID)
		}
		var rows []struct {
			PersonID int `db:"person_id"`
			denmaPersonCenter
		}
		if err := d.db().Select(&rows, strings.Join(parts, " UNION ALL ")); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		for _, r := range rows {
			byPerson[r.PersonID] = append(byPerson[r.PersonID], r.denmaPersonCenter)
		}
	}

	out := make([]denmaPersonRow, len(people))
	for i, p := range people {
		cs := byPerson[p.ID]
		sort.Slice(cs, func(i, j int) bool { return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name) })
		out[i] = denmaPersonRow{denmaPerson: p, Centers: cs}
	}
	return c.Render(http.StatusOK, "admin-denma-people", denmaPeopleView{
		adminView: newAdminView(c, "People", "", "denma.people"),
		People:    out,
	})
}

// DenmaPersonTwofaOff turns off a person's two-factor sign-in, for someone
// who has lost their phone.
func (a *App) DenmaPersonTwofaOff(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	p, err := d.person(getID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := d.setTwofa(p.ID, ""); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}
