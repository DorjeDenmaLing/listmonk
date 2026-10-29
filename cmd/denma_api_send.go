package main

// denma: the sending API's endpoints, at <center>/v1/ (cmd/denma_api.go).
// Requests and answers are JSON; answers are {"data": ...}, or {"message":
// "..."} with an error status. Unknown fields are refused, so a typo isn't
// silently ignored. A POST that sends (messages, campaigns) can carry an
// Idempotency-Key header: the same key again (from the same API key) gets
// the first request's answer and sends nothing more, so a retry after a
// timeout never sends twice.
//
// Who isn't sent to is in each answer ("skipped"), without the reason
// (denmaAPINotSent).
//
//   - Campaigns skip everyone blocklisted in the center: those who
//     unsubscribed or complained (cmd/denma_optouts.go), bounced, or whose
//     domain can't receive mail. Its recipients' domains are checked before
//     it starts (the import's check, cmd/denma_emailcheck.go), so it starts a
//     few seconds after it's made ("starting").
//   - Transactional e-mails (password resets, receipts, notices) still go to
//     those who unsubscribed, as they're not marketing; not to those who
//     complained, bounced, or whose domain can't receive mail. Each address
//     is added to the center as a subscriber without lists, if it's new, so
//     that its bounces and complaints are recorded (the e-mail carries its
//     X-Listmonk-Subscriber header).
//
// E-mail is from the key's sender (cmd/denma_api.go), and counts against its
// daily limit: a transactional e-mail when it's sent, a campaign's
// recipients when it starts.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	denmaAPIMaxTo         = 50       // addresses a transactional e-mail goes to
	denmaAPIMaxRecipients = 10_000   // recipients a campaign request adds
	denmaAPIMaxBody       = 16 << 20 // bytes
	denmaAPIMaxAttribs    = 10_000   // bytes of a recipient's attributes
	denmaAPIIdemHeader    = "Idempotency-Key"
	denmaAPIListKeepDays  = 7  // days a campaign's list is kept once it's done
	denmaAPIDraftDays     = 7  // days a campaign made with "start": false waits to be started
	denmaAPIMaxTags       = 20 // tags a Server webhook's request adds (cmd/denma_signup.go)

	denmaAPINotSent      = "not sent"                  // a skip's reason, unless the request's at fault
	denmaAPITryLater     = "not sent: try again later" // the server was too busy to take it
	denmaAPINoRecipients = "No one can be sent this campaign: it has no recipients, or every one was skipped (unsubscribed, bounced, or their domain can't receive mail). Nothing was made or sent."
)

// initDenmaAPIPublicHandlers registers the API, in a center.
func initDenmaAPIPublicHandlers(g *echo.Group, a *App) {
	if denmaHub == nil || a.ko.String("denma.center") == "" {
		return
	}
	g.GET("/v1/openapi.json", a.DenmaAPIOpenAPI) // no key (cmd/denma_api_openapi.go)
	g.POST("/v1/subscribers", a.DenmaAPISubscribersGone)
	v := g.Group("/v1", denmaAPIBody, a.apiAuth)
	v.GET("", a.DenmaAPIStatus) // GET /v1/ without the slash
	for _, r := range denmaAPIRoutes {
		h := r.Handler(a)
		if r.Can != "" {
			h = apiNeeds(r.Can, h)
		}
		v.Add(r.Method, r.Path, h)
	}
}

// denmaAPIRoutes are the API's endpoints (under /v1), each with what a key
// needs to use it ("" for any key). The OpenAPI description
// (denma_api_openapi.json) is tested against them.
var denmaAPIRoutes = []struct {
	Method, Path, Can string
	Handler           func(a *App) echo.HandlerFunc
}{
	{http.MethodGet, "/", "", func(a *App) echo.HandlerFunc { return a.DenmaAPIStatus }},
	{http.MethodGet, "/templates", "", func(a *App) echo.HandlerFunc { return a.DenmaAPITemplates }},
	{http.MethodPost, "/messages", denmaAPICanMessages, func(a *App) echo.HandlerFunc { return a.DenmaAPISendMessage }},
	{http.MethodPost, "/campaigns", denmaAPICanCampaigns, func(a *App) echo.HandlerFunc { return a.DenmaAPICreateCampaign }},
	{http.MethodGet, "/campaigns/:id", denmaAPICanCampaigns, func(a *App) echo.HandlerFunc { return a.DenmaAPIGetCampaign }},
	{http.MethodPost, "/campaigns/:id/recipients", denmaAPICanCampaigns, func(a *App) echo.HandlerFunc { return a.DenmaAPIAddRecipients }},
	{http.MethodPost, "/campaigns/:id/start", denmaAPICanCampaigns, func(a *App) echo.HandlerFunc { return a.DenmaAPIStartCampaign }},
	{http.MethodPost, "/campaigns/:id/cancel", denmaAPICanCampaigns, func(a *App) echo.HandlerFunc { return a.DenmaAPICancelCampaign }},
	{http.MethodGet, "/events", denmaAPICanEvents, func(a *App) echo.HandlerFunc { return a.DenmaAPIEvents }}, // cmd/denma_api_events.go
}

// DenmaAPISubscribersGone answers POST /v1/subscribers, which Server
// webhooks replace (cmd/denma_signup.go), whatever the key.
func (a *App) DenmaAPISubscribersGone(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	root := strings.TrimSuffix(a.urlCfg.RootURL, "/")
	return echo.NewHTTPError(http.StatusGone, fmt.Sprintf(
		"POST /v1/subscribers is gone: the API sends, and sign-up webhooks subscribe people. A superadmin makes a Server webhook on the center's Config -> Webhooks page, for %s/signup/server (described at %s/signup/openapi.json).",
		root, root))
}

// denmaAPIBody limits a request's size.
func denmaAPIBody(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, denmaAPIMaxBody)
		return next(c)
	}
}

// denmaAPIBind reads a JSON request, refusing unknown fields.
func denmaAPIBind(c echo.Context, v any) error {
	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			size := fmt.Sprintf("%d KB", mbe.Limit>>10)
			if mbe.Limit >= 1<<20 {
				size = fmt.Sprintf("%d MB", mbe.Limit>>20)
			}
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "The request is over "+size+".")
		}
		if errors.Is(err, io.EOF) {
			return echo.NewHTTPError(http.StatusBadRequest, "The request has no JSON body.")
		}
		return echo.NewHTTPError(http.StatusBadRequest, "The request isn't valid: "+err.Error())
	}
	return nil
}

// denmaAPISkip is an address not sent to, and why.
type denmaAPISkip struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// apiLog logs what a key did (no addresses), also for the activity log.
func (a *App) apiLog(c echo.Context, format string, args ...any) {
	k := apiKeyOf(c)
	msg := fmt.Sprintf(format, args...)
	a.log.Printf("denma: API key %d (%s): %s", k.ID, k.Name, msg)
	res, _ := c.Get(denmaAPIResultCtx).([]string)
	c.Set(denmaAPIResultCtx, append(res, reDenmaAddress.ReplaceAllString(msg, "(an address)"))) // the activity log's
}

// apiTemplate finds a template by name, of one of the types.
func (a *App) apiTemplate(name string, kind string, types ...string) (int, string, error) {
	var rows []struct {
		ID   int    `db:"id"`
		Type string `db:"type"`
	}
	if err := a.db.Select(&rows, `SELECT id, type::TEXT AS type FROM templates WHERE name = $1 AND type::TEXT = ANY($2) ORDER BY id`,
		strings.TrimSpace(name), pq.Array(types)); err != nil {
		return 0, "", echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	switch len(rows) {
	case 1:
		return rows[0].ID, rows[0].Type, nil
	case 0:
		var names []string
		if err := a.db.Select(&names, `SELECT name FROM templates WHERE type::TEXT = ANY($1) ORDER BY name`, pq.Array(types)); err != nil {
			return 0, "", echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		have := "The center has none yet: make one under Campaigns -> Templates."
		if len(names) > 0 {
			have = "The center's are: " + strings.Join(names, ", ") + "."
		}
		return 0, "", echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("There's no %s template called %q. %s", kind, name, have))
	}
	return 0, "", echo.NewHTTPError(http.StatusBadRequest,
		fmt.Sprintf("%d %s templates are called %q; give them different names (Campaigns -> Templates).", len(rows), kind, name))
}

// apiSender returns the key's sender (or the center's), checked as ready to
// send from, and the Reply-To value, which has to be at the center's domains.
func (a *App) apiSender(c echo.Context, replyTo string) (string, string, error) {
	from := apiKeyOf(c).Sender
	if from == "" {
		from = a.cfg.FromEmail
	}
	if err := a.denmaCheckSenderReady(from); err != nil { // cmd/denma_domains.go
		return "", "", denmaBadRequest(err)
	}
	replyTo = strings.TrimSpace(replyTo)
	if replyTo != "" {
		addr, err := mail.ParseAddress(replyTo)
		if err != nil {
			return "", "", echo.NewHTTPError(http.StatusBadRequest, `"reply_to" isn't a valid address.`)
		}
		if err := a.apiReplyToOK(addr.Address); err != nil {
			return "", "", err
		}
		replyTo = addr.String()
	}
	return from, replyTo, nil
}

// apiReplyToOK refuses a reply-to address that isn't at one of the center's
// sending domains (or its sender's, without them, cmd/denma_domains.go).
func (a *App) apiReplyToOK(addr string) error {
	dom := denmaSenderDomain(addr)
	doms, err := a.denmaCenterDomains()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(doms) == 0 {
		if dom == denmaSenderDomain(a.cfg.FromEmail) || a.denmaCenterOf() == nil || !denmaDomainsOn() {
			return nil
		}
	}
	names := make([]string, 0, len(doms))
	for _, d := range doms {
		if d.Domain == dom {
			return nil
		}
		names = append(names, d.Domain)
	}
	if len(names) == 0 {
		names = append(names, denmaSenderDomain(a.cfg.FromEmail))
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"reply_to" has to be at one of the center's domains (%s).`, strings.Join(names, ", ")))
}

// apiCapErr is the answer when the center's daily cap leaves no room for n
// e-mails.
func (a *App) apiCapErr(n int) error {
	s := denmaCenterCounter(a.ko).Status()
	if s.Left > 0 {
		return echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf(
			"The center's daily cap of %d e-mails leaves room for %d more now (%d sent in the last 24 hours), not %d. Send fewer, or try again later.",
			s.Limit, s.Left, s.Sent, n))
	}
	when := "later"
	if !s.FreesAt.IsZero() {
		when = "after " + s.FreesAt.UTC().Format(time.RFC3339)
	}
	return echo.NewHTTPError(http.StatusTooManyRequests,
		fmt.Sprintf("The center's daily cap of %d e-mails is reached (%d sent in the last 24 hours). Try again %s.", s.Limit, s.Sent, when))
}

// ---- GET /v1 ----

// DenmaAPIStatus says which center and key this is, and how its sending
// stands: a request to test a key with.
func (a *App) DenmaAPIStatus(c echo.Context) error {
	st, err := a.apiState()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	k := apiKeyOf(c)
	out := map[string]any{
		"center":        a.ko.String("app.site_name"),
		"key":           k.Name,
		"can":           k.Can,
		"expires_at":    k.ExpiresAt,
		"sending":       a.denmaCapStatus(),
		"paused":        st.PausedAt != nil,
		"paused_reason": st.PausedReason,
	}
	if denmaAPISends(k.Can) {
		var used int
		if err := a.db.Get(&used, `SELECT COALESCE(SUM(n), 0)::INT FROM denma_api_usage
			WHERE key_id = $1 AND kind = $2 AND created_at > NOW() - INTERVAL '24 hours'`, k.ID, denmaAPIUseSend); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		from := k.Sender
		if from == "" {
			from = a.cfg.FromEmail
		}
		out["sender"] = from
		out["key_sending"] = map[string]int{"daily_limit": k.DailyLimit, "sent_24h": used, "left": max(k.DailyLimit-used, 0)}
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaAPITemplates lists the templates the API can name.
func (a *App) DenmaAPITemplates(c echo.Context) error {
	out, err := a.apiTemplates()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// ---- POST /v1/messages ----

// denmaAPITo is one address, or a list.
type denmaAPITo []string

func (t *denmaAPITo) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = denmaAPITo{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New(`"to" has to be an address or a list of addresses`)
	}
	*t = many
	return nil
}

type denmaAPIMessageReq struct {
	Template string         `json:"template"`
	To       denmaAPITo     `json:"to"`
	Name     string         `json:"name"`
	First    string         `json:"first_name"`
	Last     string         `json:"last_name"`
	Data     map[string]any `json:"data"`
	Subject  string         `json:"subject"`
	ReplyTo  string         `json:"reply_to"`
}

type denmaAPIMessageResult struct {
	Sent    []string       `json:"sent"`
	Skipped []denmaAPISkip `json:"skipped"`
}

// apiIdem starts an idempotent request: done is the stored answer if the key
// was used before; else id is the row to store this one's in (0 without a
// key).
func (a *App) apiIdem(c echo.Context, table string) (id int64, done json.RawMessage, err error) {
	idem := strings.TrimSpace(c.Request().Header.Get(denmaAPIIdemHeader))
	if idem == "" {
		return 0, nil, nil
	}
	if len(idem) > 200 {
		return 0, nil, echo.NewHTTPError(http.StatusBadRequest, "The Idempotency-Key is over 200 characters.")
	}
	k := apiKeyOf(c)
	switch table {
	case "messages":
		err = a.db.Get(&id, `INSERT INTO denma_api_messages (key_id, idempotency_key) VALUES ($1, $2)
			ON CONFLICT (key_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING RETURNING id`, k.ID, idem)
		if errors.Is(err, sql.ErrNoRows) {
			var res json.RawMessage
			if err := a.db.Get(&res, `SELECT result FROM denma_api_messages WHERE key_id = $1 AND idempotency_key = $2`, k.ID, idem); err != nil {
				return 0, nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if string(res) == "{}" {
				return 0, nil, echo.NewHTTPError(http.StatusConflict, "A request with this Idempotency-Key is still being handled.")
			}
			return 0, res, nil
		}
	}
	if err != nil {
		return 0, nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return id, nil, nil
}

// DenmaAPISendMessage sends a transactional e-mail.
func (a *App) DenmaAPISendMessage(c echo.Context) error {
	var r denmaAPIMessageReq
	if err := denmaAPIBind(c, &r); err != nil {
		return err
	}
	if err := a.apiCheckLinks(c, r.Subject, r.Data); err != nil {
		return err
	}
	if err := a.apiCanSend(); err != nil {
		return err
	}
	if len(r.To) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, `Give "to": an address, or a list of them.`)
	}
	if len(r.To) > denmaAPIMaxTo {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"to" can have at most %d addresses; send a campaign for more.`, denmaAPIMaxTo))
	}
	tplID, _, err := a.apiTemplate(r.Template, "transactional", "tx")
	if err != nil {
		return err
	}
	tpl, err := a.manager.GetTpl(tplID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "The template isn't loaded: "+err.Error())
	}
	from, replyTo, err := a.apiSender(c, r.ReplyTo)
	if err != nil {
		return err
	}
	if len(r.Subject) > 1000 {
		return echo.NewHTTPError(http.StatusBadRequest, `"subject" is over 1,000 characters.`)
	}
	name := denmaSignupField(map[string]string{"n": r.Name}, "n", 200)
	first, last := denmaTidyName(r.First), denmaTidyName(r.Last) // cmd/denma_names.go
	if n := denmaJoinName(first, last); n != "" {
		name = n
	}
	if denmaNameHasLink(name) {
		return echo.NewHTTPError(http.StatusBadRequest, `"name" can't have a link in it.`)
	}

	idemID, done, err := a.apiIdem(c, "messages")
	if err != nil {
		return err
	}
	if done != nil {
		return c.JSON(http.StatusOK, okResp{done})
	}
	// Without a result, the Idempotency-Key's row is forgotten, so the
	// request can be tried again.
	stored := false
	defer func() {
		if idemID != 0 && !stored {
			_, _ = a.db.Exec(`DELETE FROM denma_api_messages WHERE id = $1`, idemID)
		}
	}()

	// Who to send to.
	res := denmaAPIMessageResult{Sent: []string{}, Skipped: []denmaAPISkip{}}
	skip := denmaEmailSkip(a.ko)
	// Each address given, of the key's daily limit, before anyone's added.
	named := map[string]bool{}
	for _, to := range r.To {
		if email, err := a.importer.SanitizeEmail(strings.TrimSpace(to)); err == nil {
			named[strings.ToLower(email)] = true
		}
	}
	if err := a.apiLimitClaim(c, denmaAPIUseNamed, "addresses", len(named)); err != nil {
		return err
	}
	var subs []models.Subscriber
	seen := map[string]bool{}
	for _, to := range r.To {
		email, err := a.importer.SanitizeEmail(strings.TrimSpace(to))
		if err != nil {
			res.Skipped = append(res.Skipped, denmaAPISkip{to, "not a valid address"})
			continue
		}
		email = strings.ToLower(email)
		if seen[email] {
			continue
		}
		seen[email] = true
		sub, why, err := a.apiTxRecipient(apiKeyOf(c).ID, email, name, first, last, skip)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if why != "" {
			res.Skipped = append(res.Skipped, denmaAPISkip{email, why})
			continue
		}
		subs = append(subs, sub)
	}

	// Each one's message, before anything is sent.
	msgs := make([]models.Message, 0, len(subs))
	for _, sub := range subs {
		m := models.TxMessage{TemplateID: tplID, Data: r.Data, Subject: r.Subject, FromEmail: from}
		if m.Data == nil {
			m.Data = map[string]any{}
		}
		if err := m.Render(sub, tpl, a.manager.GenericTemplateFuncs()); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "The template couldn't be filled in: "+err.Error())
		}
		// Its links as filled in, from the template's logic too.
		if err := denmaCheckRendered(append(a.apiOwnLinkDomains(), apiKeyOf(c).LinkDomains...), tpl.Subject+tpl.Body,
			m.Subject, string(m.Body), m.AltBody); err != nil {
			return err
		}
		if replyTo == "" {
			replyTo = a.denmaReplyTo() // the center's default (cmd/denma_reply_to.go)
		}
		msgs = append(msgs, denmaAPIMessage(sub, from, replyTo, m.Subject, m.ContentType, m.Body, m.AltBody, tpl.Attachments)) // cmd/denma_api_outbox.go
	}

	// The key's daily limit, and the center's daily cap: all of them, or
	// none.
	k := apiKeyOf(c)
	if len(msgs) > 0 {
		if err := a.apiSendClaim(c, len(msgs)); err != nil {
			return err
		}
	}
	cc := denmaCenterCounter(a.ko)
	var centerClaim int64
	if cc != nil && len(msgs) > 0 {
		id, ok := cc.ClaimAll(len(msgs))
		if !ok {
			a.apiUnclaim(k.ID, denmaAPIUseSend, len(msgs))
			return a.apiCapErr(len(msgs))
		}
		centerClaim = id
	}
	// And the hub's daily limit, short of the reserve kept for opt-ins and
	// password resets (cmd/denma_sending.go).
	daily, err := denmaClaimDaily(len(msgs))
	if err != nil {
		if cc != nil {
			cc.Release(centerClaim, len(msgs))
		}
		a.apiUnclaim(k.ID, denmaAPIUseSend, len(msgs))
		return err
	}
	for i, msg := range msgs {
		denmaMarkClaim(&msg, denmaCenterClaimHeader, centerClaim) // cmd/denma_daily.go
		denmaMarkClaim(&msg, denmaClaimHeader, daily)
		// Recorded until the mail server answers, which may mean trying it
		// again or an event for the key (cmd/denma_api_outbox.go).
		outID, err := a.apiOutboxAdd(k.ID, tplID, msg)
		if err == nil {
			if err = a.apiOutboxPush(outID, msg); err != nil {
				a.apiOutboxForget(outID)
			}
		}
		if err != nil {
			if cc != nil {
				cc.Release(centerClaim, 1)
			}
			denmaDaily.Release(daily, 1)
			res.Skipped = append(res.Skipped, denmaAPISkip{subs[i].Email, denmaAPITryLater})
			continue
		}
		res.Sent = append(res.Sent, subs[i].Email)
	}
	a.apiUnclaim(k.ID, denmaAPIUseSend, len(msgs)-len(res.Sent))
	if err := denmaAPIContacts(a.db, k.ID, res.Sent); err != nil { // its events (cmd/denma_api_events.go)
		a.log.Printf("denma: error recording API key %d's addresses: %v", k.ID, err)
	}

	b, _ := json.Marshal(res)
	if idemID != 0 {
		_, err = a.db.Exec(`UPDATE denma_api_messages SET template_id = $2, sent = $3, skipped = $4, result = $5 WHERE id = $1`,
			idemID, tplID, len(res.Sent), len(res.Skipped), string(b))
		stored = err == nil
	} else {
		_, err = a.db.Exec(`INSERT INTO denma_api_messages (key_id, template_id, sent, skipped, result) VALUES ($1, $2, $3, $4, $5)`,
			k.ID, tplID, len(res.Sent), len(res.Skipped), string(b))
	}
	if err != nil {
		a.log.Printf("denma: error recording an API e-mail: %v", err)
	}
	a.apiLog(c, "transactional e-mail %q: %d sent, %d skipped", r.Template, len(res.Sent), len(res.Skipped))
	return c.JSON(http.StatusOK, okResp{res})
}

// apiTxRecipient readies a transactional e-mail's recipient: the subscriber
// (added without lists if new, so that bounces and complaints are recorded,
// and with no consent, cmd/denma_noconsent.go), or denmaAPINotSent if it's
// not sent to.
func (a *App) apiTxRecipient(keyID int, email, name, first, last string, skip map[string]bool) (models.Subscriber, string, error) {
	var sub models.Subscriber
	if no, err := a.denmaTxRefused(email, skip); err != nil || no {
		return sub, denmaAPINotSent, err
	}

	get := func() error {
		return a.db.Get(&sub, `SELECT id, uuid, email, name, first_name, last_name, attribs, status, created_at, updated_at FROM subscribers WHERE LOWER(email) = $1`, email)
	}
	err := get()
	if errors.Is(err, sql.ErrNoRows) {
		if name == "" {
			name = strings.Split(email, "@")[0]
		}
		var added []string
		if err := a.db.Select(&added, `INSERT INTO subscribers (uuid, email, name, first_name, last_name, status) VALUES (gen_random_uuid(), $1, $2, $3, $4, 'enabled')
			ON CONFLICT DO NOTHING RETURNING LOWER(email)`, email, name, first, last); err != nil {
			return sub, "", err
		}
		if err := denmaNoConsentAdd(a.db, keyID, added); err != nil {
			return sub, "", err
		}
		err = get()
	}
	return sub, "", err
}

// denmaTxRefused reports whether a transactional e-mail isn't sent to email:
// it complained here, hard-bounced (here, which the shared blocklist only
// has after the minute's job, cmd/denma_blocklist.go, or anywhere), or its
// domain can't receive mail. Someone who unsubscribed, or whom an admin
// blocklisted, is still sent it.
func (a *App) denmaTxRefused(email string, skip map[string]bool) (bool, error) {
	if _, ok := denmaSharedBlock(a.db, email); ok {
		return true, nil
	}
	var kind string
	if err := a.db.Get(&kind, `SELECT COALESCE(denma_optout_kind($1), '')`, email); err != nil {
		return false, err
	}
	if kind == denmaOptOutComplained {
		return true, nil
	}
	if bad, reason := denmaEmailBad(email, skip); bad {
		denmaBlocklist(a.db, email, reason)
		denmaBlockEverywhere(email, reason, a.ko.String("denma.center"))
		return true, nil
	}
	var no bool
	err := a.db.Get(&no, `SELECT EXISTS (SELECT 1 FROM subscribers s WHERE LOWER(s.email) = LOWER($1) AND s.status = 'blocklisted'
		AND (COALESCE(s.attribs->>'auto_blocklist_reason', '') <> ''
			OR EXISTS (SELECT 1 FROM bounces b WHERE b.subscriber_id = s.id AND b.type = 'hard')))`, email)
	return no, err
}

// denmaTxSkipped reports whether listmonk's transactional API (/api/tx)
// leaves email out (denmaTxRefused), adding why to notFound.
func (a *App) denmaTxSkipped(email string, notFound *[]string) bool {
	no, err := a.denmaTxRefused(email, denmaEmailSkip(a.ko))
	if err != nil {
		a.log.Printf("denma: error checking a transactional e-mail's recipient: %v", err)
	}
	if no || err != nil {
		*notFound = append(*notFound, email+" isn't sent e-mail: they marked one as spam, it bounced, or its domain can't receive mail")
	}
	return no || err != nil
}

// ---- POST /v1/campaigns ----

type denmaAPIRecipient struct {
	Email   string         `json:"email"`
	Name    string         `json:"name"`
	First   string         `json:"first_name"`
	Last    string         `json:"last_name"`
	Attribs map[string]any `json:"attribs"`
}

type denmaAPICampaignReq struct {
	Name       string              `json:"name"`
	Template   string              `json:"template"`
	Subject    string              `json:"subject"`
	ReplyTo    string              `json:"reply_to"`
	Body       string              `json:"body"`
	Data       map[string]any      `json:"data"`
	Tags       []string            `json:"tags"`
	SendAt     *time.Time          `json:"send_at"`
	Start      *bool               `json:"start"`
	Recipients []denmaAPIRecipient `json:"recipients"`
}

// denmaAPICampaign is an API campaign, as GET /v1/campaigns/:id answers.
type denmaAPICampaign struct {
	ID         int        `db:"id" json:"id"`
	UUID       string     `db:"uuid" json:"uuid"`
	Name       string     `db:"name" json:"name"`
	Subject    string     `db:"subject" json:"subject"`
	Status     string     `db:"status" json:"status"`
	Error      string     `db:"error" json:"error,omitempty"`
	Recipients int        `db:"recipients" json:"recipients"`
	ToSend     int        `db:"to_send" json:"to_send"`
	Sent       int        `db:"sent" json:"sent"`
	Failed     int        `db:"failed" json:"failed"`
	Opens      int        `db:"opens" json:"opens"`
	Clicks     int        `db:"clicks" json:"clicks"`
	Bounces    int        `db:"bounces" json:"bounces"`
	SendAt     *time.Time `db:"send_at" json:"send_at"`
	StartedAt  *time.Time `db:"started_at" json:"started_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at"`

	Skipped []denmaAPISkip `db:"-" json:"skipped,omitempty"` // when recipients are added, and who won't be sent it when it starts
}

// A draft's recipients are everyone given: the count doesn't say who the
// center has blocklisted until it starts (apiNotSent).
const denmaAPICampaignSQL = `SELECT c.id, c.uuid, c.name, c.subject, x.error,
	CASE WHEN x.state = 'starting' AND c.status = 'draft' THEN 'starting' ELSE c.status::TEXT END AS status,
	(SELECT COUNT(*) FROM subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id
		WHERE sl.list_id = x.list_id AND (c.status = 'draft' OR (sl.status <> 'unsubscribed' AND s.status <> 'blocklisted'))) AS recipients,
	c.to_send, c.sent,
	(SELECT COUNT(*) FROM denma_send_failures f WHERE f.campaign_id = c.id) AS failed,
	(SELECT COUNT(DISTINCT v.subscriber_id) FROM campaign_views v WHERE v.campaign_id = c.id) AS opens,
	(SELECT COUNT(DISTINCT l.subscriber_id) FROM link_clicks l WHERE l.campaign_id = c.id) AS clicks,
	(SELECT COUNT(*) FROM bounces b WHERE b.campaign_id = c.id) AS bounces,
	c.send_at, c.started_at, c.created_at, c.updated_at
	FROM denma_api_campaigns x JOIN campaigns c ON c.id = x.campaign_id`

// apiCampaign is one of the key's campaigns.
func (a *App) apiCampaign(c echo.Context, id int) (denmaAPICampaign, error) {
	var out denmaAPICampaign
	if err := a.db.Get(&out, denmaAPICampaignSQL+` WHERE x.campaign_id = $1 AND x.key_id = $2`, id, apiKeyOf(c).ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, echo.NewHTTPError(http.StatusNotFound, "This key has no campaign with that ID.")
		}
		return out, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return out, nil
}

// DenmaAPICreateCampaign makes a campaign, with its recipients, and starts
// it unless "start" is false.
func (a *App) DenmaAPICreateCampaign(c echo.Context) error {
	var r denmaAPICampaignReq
	if err := denmaAPIBind(c, &r); err != nil {
		return err
	}
	if err := a.apiCheckLinks(c, r.Subject, r.Body, r.Data, denmaAPIRecipientAttribs(r.Recipients)); err != nil {
		return err
	}
	start := r.Start == nil || *r.Start
	if start {
		if err := a.apiCanSend(); err != nil {
			return err
		}
	}
	k := apiKeyOf(c)

	// The same Idempotency-Key again: the campaign it made.
	idem := strings.TrimSpace(c.Request().Header.Get(denmaAPIIdemHeader))
	if len(idem) > 200 {
		return echo.NewHTTPError(http.StatusBadRequest, "The Idempotency-Key is over 200 characters.")
	}
	if idem != "" {
		var id int
		err := a.db.Get(&id, `SELECT campaign_id FROM denma_api_campaigns WHERE key_id = $1 AND idempotency_key = $2`, k.ID, idem)
		if err == nil {
			out, err := a.apiCampaign(c, id)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusOK, okResp{out})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	if len(r.Recipients) > denmaAPIMaxRecipients {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
			"At most %d recipients a request. Make the campaign with \"start\": false, add the rest with POST /v1/campaigns/{id}/recipients, then start it.", denmaAPIMaxRecipients))
	}
	if start && len(r.Recipients) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, `Give "recipients", or "start": false to add them later.`)
	}
	tplID, tplType, err := a.apiTemplate(r.Template, "campaign", "campaign", "campaign_visual")
	if err != nil {
		return err
	}
	from, replyTo, err := a.apiSender(c, r.ReplyTo)
	if err != nil {
		return err
	}
	if r.SendAt != nil && !r.SendAt.After(time.Now()) {
		return echo.NewHTTPError(http.StatusBadRequest, `"send_at" has to be in the future; leave it out to send now.`)
	}
	if tplType == "campaign_visual" && r.Body != "" {
		return echo.NewHTTPError(http.StatusBadRequest, `A visual template is the whole e-mail: leave "body" out, or use a campaign template whose {{ template "content" . }} it fills.`)
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = strings.TrimSpace(r.Subject)
	}
	if name == "" || strings.Contains(name, "{{") {
		name = "API campaign, " + time.Now().UTC().Format("2006-01-02 15:04 UTC")
	}

	// A campaign, of the key's daily limit, drafts too.
	if err := a.apiLimitClaim(c, denmaAPIUseCampaign, "campaigns", 1); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			a.apiUnclaim(k.ID, denmaAPIUseCampaign, 1)
		}
	}()

	// Its list, and its recipients on it.
	listID, err := a.apiNewCampaignList("API: "+name,
		fmt.Sprintf("The recipients of a campaign sent through the API (key %q). Deleted %d days after the campaign is done.", k.Name, denmaAPIListKeepDays))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer func() {
		if !keep {
			_, _ = a.db.Exec(`DELETE FROM lists WHERE id = $1`, listID)
		}
	}()
	skipped, err := a.apiAddRecipients(c, listID, r.Recipients)
	if err != nil {
		return err
	}
	claimed := 0
	if start {
		// Who it won't go to, said only once it's sent: a draft's answer
		// mustn't tell a key who the center has blocklisted for free.
		if skipped, err = a.apiNotSent(listID, skipped); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		n, err := a.apiListRecipients(listID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if n == 0 {
			return c.JSON(http.StatusBadRequest, map[string]any{"message": denmaAPINoRecipients, "skipped": skipped})
		}
		// Its recipients, of the key's daily limit.
		if err := a.apiSendClaim(c, n); err != nil {
			return err
		}
		claimed = n
		defer func() {
			if !keep {
				a.apiUnclaim(k.ID, denmaAPIUseSend, claimed)
			}
		}()
	}

	// The campaign, checked as the admin's are.
	o := campReq{ListIDs: []int{listID}}
	o.Type = models.CampaignTypeRegular
	o.Name = name
	o.Subject = r.Subject
	o.FromEmail = from
	o.Body = r.Body
	o.ContentType = models.CampaignContentTypeHTML
	if tplType == "campaign_visual" {
		o.ContentType = models.CampaignContentTypeVisual
	}
	o.TemplateID = null.IntFrom(tplID)
	o.Messenger = emailMsgr
	o.Tags = append(denmaNormTags(r.Tags), "api")
	o.Headers = models.Headers{}
	o.ReplyTo = replyTo // else the center's default (cmd/denma_reply_to.go)
	o.Attribs = models.JSON{}
	if len(r.Data) > 0 {
		o.Attribs["data"] = r.Data // {{ .Campaign.Attribs.data.<name> }}
	}
	o.ArchiveMeta = json.RawMessage("{}")
	if r.SendAt != nil {
		o.SendAt = null.TimeFrom(*r.SendAt)
	}
	if o, err = a.validateCampaignFields(o); err != nil {
		return denmaBadRequest(err)
	}
	camp, err := a.core.CreateCampaign(o.Campaign, o.ListIDs, nil)
	if err != nil {
		return err
	}
	if err := a.denmaSaveCampaign(camp.ID, &o, &camp); err != nil { // its reply-to (cmd/denma_reply_to.go)
		_ = a.core.DeleteCampaign(camp.ID)
		return err
	}
	state := "draft"
	if start {
		state = "starting"
	}
	var idemArg any
	if idem != "" {
		idemArg = idem
	}
	// Recorded only while the key's in use: one deleted as leaked since this
	// request came in has had its campaigns cancelled (cmd/denma_api.go).
	kl := a.apiKeyLock(k.ID)
	kl.Lock()
	live, err := a.apiKeyLive(k.ID, false)
	if err == nil && !live {
		kl.Unlock()
		_ = a.core.DeleteCampaign(camp.ID)
		return echo.NewHTTPError(http.StatusUnauthorized, "This key isn't in use (any more).")
	}
	if err == nil {
		_, err = a.db.Exec(`INSERT INTO denma_api_campaigns (campaign_id, key_id, list_id, idempotency_key, state, claimed) VALUES ($1, $2, $3, $4, $5, $6)`,
			camp.ID, k.ID, listID, idemArg, state, claimed)
	}
	kl.Unlock()
	if err != nil {
		_ = a.core.DeleteCampaign(camp.ID)
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "23505" { // the same Idempotency-Key, at the same time
			return echo.NewHTTPError(http.StatusConflict, "A request with this Idempotency-Key is still being handled.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	keep = true
	if start {
		go a.apiStart(camp.ID)
	}

	out, err := a.apiCampaign(c, camp.ID)
	if err != nil {
		return err
	}
	out.Skipped = skipped
	a.apiLog(c, "campaign %d made with template %q: %d recipients, %d skipped", camp.ID, r.Template, out.Recipients, len(skipped))
	return c.JSON(http.StatusOK, okResp{out})
}

// apiAddRecipients adds recipients to an API campaign's list, creating new
// subscribers, and returns those skipped for the request's faults (not a
// valid address, attributes too big): the blocklisted go on the list, not
// to be sent it, and are said when it starts (apiNotSent). Their attributes are
// stored in the list's subscriber_lists.meta, for this campaign only
// (denmaAPIRecipientData); existing subscribers keep theirs, and their name
// unless it's a stand-in, which the name given replaces as the campaign
// starts (denma_api_campaign_contacts), not for a draft never sent.
func (a *App) apiAddRecipients(c echo.Context, listID int, in []denmaAPIRecipient) ([]denmaAPISkip, error) {
	keyID := apiKeyOf(c).ID
	skipped := []denmaAPISkip{}
	var emails, names, firsts, lasts, attribs []string
	seen := map[string]bool{}
	for _, r := range in {
		if k := denmaReservedAttrib(r.Attribs, false); k != "" {
			return nil, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Recipient %s: %s", r.Email, denmaReservedAttribMsg(k)))
		}
		email, err := a.importer.SanitizeEmail(strings.TrimSpace(r.Email))
		if err != nil {
			skipped = append(skipped, denmaAPISkip{r.Email, "not a valid address"})
			continue
		}
		email = strings.ToLower(email)
		if seen[email] {
			continue
		}
		seen[email] = true
		attr := []byte("{}")
		if len(r.Attribs) > 0 {
			attr, _ = json.Marshal(r.Attribs)
			if len(attr) > denmaAPIMaxAttribs {
				skipped = append(skipped, denmaAPISkip{email, fmt.Sprintf("its attributes are over %d bytes", denmaAPIMaxAttribs)})
				continue
			}
		}
		name := denmaSignupField(map[string]string{"n": r.Name}, "n", 200)
		first, last := denmaTidyName(r.First), denmaTidyName(r.Last) // cmd/denma_names.go
		if n := denmaJoinName(first, last); n != "" {
			name = n
		}
		if denmaNameHasLink(name) {
			return nil, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Recipient %s: a name can't have a link in it.", email))
		}
		emails = append(emails, email)
		names = append(names, name)
		firsts, lasts = append(firsts, first), append(lasts, last)
		attribs = append(attribs, string(attr))
	}
	if len(emails) == 0 {
		return skipped, nil
	}
	// Each address given, of the key's daily limit, before anyone's added.
	if err := a.apiLimitClaim(c, denmaAPIUseNamed, "addresses", len(emails)); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			a.apiUnclaim(keyID, denmaAPIUseNamed, len(emails))
		}
	}()

	fail := func(err error) ([]denmaAPISkip, error) {
		a.log.Printf("denma: error adding an API campaign's recipients: %v", err)
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "The recipients couldn't be added: "+err.Error())
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	// New ones, with their attributes, written once: existing subscribers
	// keep theirs. An address that opted out is added blocklisted, without
	// them. None has consent (cmd/denma_noconsent.go).
	var added []string
	if err := tx.Select(&added, `INSERT INTO subscribers (uuid, email, name, first_name, last_name, status, attribs)
		SELECT gen_random_uuid(), i.email, COALESCE(NULLIF(i.name, ''), split_part(i.email, '@', 1)), i.first, i.last, 'enabled',
			CASE WHEN denma_optout_kind(i.email) IS NOT NULL THEN '{}'::JSONB ELSE i.attribs END
		FROM unnest($1::TEXT[], $2::TEXT[], $3::JSONB[], $4::TEXT[], $5::TEXT[]) AS i(email, name, attribs, first, last)
		ON CONFLICT DO NOTHING RETURNING LOWER(email)`, pq.Array(emails), pq.Array(names), pq.Array(attribs), pq.Array(firsts), pq.Array(lasts)); err != nil {
		return fail(err)
	}
	if err := denmaNoConsentAdd(tx, keyID, added); err != nil {
		return fail(err)
	}
	var subs []struct {
		ID     int    `db:"id"`
		Email  string `db:"email"`
		Status string `db:"status"`
	}
	if err := tx.Select(&subs, `SELECT s.id, LOWER(s.email) AS email, s.status::TEXT AS status
		FROM subscribers s WHERE LOWER(s.email) = ANY($1)`, pq.Array(emails)); err != nil {
		return fail(err)
	}
	var ids []int
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	// On the list, with their attributes and name; the latest if given again. Only
	// here can the fenced join it (cmd/denma_noconsent.go).
	if _, err := tx.Exec(`SELECT set_config('denma.api_recipients', $1, true)`, strconv.Itoa(listID)); err != nil {
		return fail(err)
	}
	if _, err := tx.Exec(`INSERT INTO subscriber_lists (subscriber_id, list_id, status, meta)
		SELECT s.id, $2, 'confirmed', (CASE WHEN i.attribs = '{}'::JSONB THEN '{}'::JSONB ELSE jsonb_build_object('attribs', i.attribs) END)
			|| (CASE WHEN i.name = '' THEN '{}'::JSONB ELSE jsonb_build_object('name', i.name) END)
			|| (CASE WHEN i.first = '' AND i.last = '' THEN '{}'::JSONB ELSE jsonb_build_object('first_name', i.first, 'last_name', i.last) END)
		FROM unnest($3::TEXT[], $4::JSONB[], $5::TEXT[], $6::TEXT[], $7::TEXT[]) AS i(email, attribs, name, first, last) JOIN subscribers s ON LOWER(s.email) = i.email
		WHERE s.id = ANY($1::INT[])
		ON CONFLICT (subscriber_id, list_id) DO UPDATE SET meta = EXCLUDED.meta, updated_at = NOW()`,
		pq.Array(ids), listID, pq.Array(emails), pq.Array(attribs), pq.Array(names), pq.Array(firsts), pq.Array(lasts)); err != nil {
		return fail(err)
	}
	// The key's contacts are recorded as it starts (denma_api_campaign_contacts).
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	committed = true
	return skipped, nil
}

// apiNotSent adds to skipped those on an API campaign's list who won't be
// sent it: blocklisted, or unsubscribed from it.
func (a *App) apiNotSent(listID int, skipped []denmaAPISkip) ([]denmaAPISkip, error) {
	var emails []string
	if err := a.db.Select(&emails, `SELECT LOWER(s.email) FROM subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id
		WHERE sl.list_id = $1 AND (sl.status = 'unsubscribed' OR s.status = 'blocklisted') ORDER BY 1`, listID); err != nil {
		return nil, err
	}
	for _, e := range emails {
		skipped = append(skipped, denmaAPISkip{e, denmaAPINotSent})
	}
	return skipped, nil
}

// apiNewCampaignList makes an API campaign's list, with its tag
// (denmaAPIListTag), which only the API can give a list
// (denma_keep_api_list_tag, cmd/denma_noconsent.go).
func (a *App) apiNewCampaignList(name, desc string) (int, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT set_config('denma.api_list', 'on', true)`); err != nil {
		return 0, err
	}
	var id int
	if err := tx.Get(&id, `INSERT INTO lists (uuid, name, type, optin, tags, description)
		VALUES (gen_random_uuid(), $1, 'private', 'single', $2, $3) RETURNING id`,
		name, pq.Array([]string{denmaAPIListTag}), desc); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// denmaAPIRecipientData overlays an API campaign's per-recipient attributes
// (apiAddRecipients) on its subscribers, for its e-mails: the manager's
// batches (cmd/manager_store.go) and retries (cmd/denma_retries.go).
func denmaAPIRecipientData(db *sqlx.DB, campID int, subs []models.Subscriber) error {
	if len(subs) == 0 {
		return nil
	}
	ids := make([]int64, len(subs))
	for i, s := range subs {
		ids[i] = int64(s.ID)
	}
	var rows []struct {
		ID      int    `db:"subscriber_id"`
		Attribs []byte `db:"attribs"`
	}
	if err := db.Select(&rows, `SELECT sl.subscriber_id, sl.meta->'attribs' AS attribs FROM denma_api_campaigns x
		JOIN subscriber_lists sl ON sl.list_id = x.list_id
		WHERE x.campaign_id = $1 AND sl.subscriber_id = ANY($2) AND jsonb_typeof(sl.meta->'attribs') = 'object'`,
		campID, pq.Array(ids)); err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "42P01" {
			return nil // no API here (undefined table)
		}
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	add := make(map[int]map[string]any, len(rows))
	for _, r := range rows {
		m := map[string]any{}
		if err := json.Unmarshal(r.Attribs, &m); err == nil {
			add[r.ID] = m
		}
	}
	for i := range subs {
		a := add[subs[i].ID]
		if a == nil {
			continue
		}
		m := make(models.JSON, len(subs[i].Attribs)+len(a))
		for k, v := range subs[i].Attribs {
			m[k] = v
		}
		for k, v := range a {
			m[k] = v
		}
		subs[i].Attribs = m
	}
	return nil
}

// denmaViewRecipientData is a subscriber with an API campaign's attributes
// for them laid over, for the campaign's "view in browser" page, as its
// e-mail had them.
func (a *App) denmaViewRecipientData(campID int, sub models.Subscriber) models.Subscriber {
	subs := []models.Subscriber{sub}
	if err := denmaAPIRecipientData(a.db, campID, subs); err != nil {
		a.log.Printf("denma: error reading API campaign %d's recipient data: %v", campID, err)
		return sub
	}
	return subs[0]
}

// apiListRecipients is how many on an API campaign's list can be sent it.
func (a *App) apiListRecipients(listID int) (int, error) {
	var n int
	err := a.db.Get(&n, `SELECT COUNT(*) FROM subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id
		WHERE sl.list_id = $1 AND sl.status <> 'unsubscribed' AND s.status <> 'blocklisted'`, listID)
	return n, err
}

// denmaAPIStarting is the campaigns being started, so that the minute's job
// (cmd/denma_api_events.go) doesn't start one twice.
var denmaAPIStarting sync.Map

// apiStart starts an API campaign: its recipients' domains (and the shared
// blocklist) checked, then running, or scheduled for its send_at.
func (a *App) apiStart(id int) {
	key := fmt.Sprintf("%s/%d", a.ko.String("denma.center"), id)
	if _, busy := denmaAPIStarting.LoadOrStore(key, true); busy {
		return
	}
	defer denmaAPIStarting.Delete(key)
	// While the API's paused, it stays starting; the minute's job starts it
	// once it's resumed.
	if st, err := a.apiState(); err != nil || st.PausedAt != nil {
		return
	}

	failed := func(err error) {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			err = fmt.Errorf("%v", he.Message) // without echo's "code=...", for the caller
		}
		a.log.Printf("denma: API campaign %d didn't start: %v", id, err)
		// A draft again, its claim on the key's daily limit given back: starting
		// it again claims it again.
		var back struct {
			KeyID   sql.NullInt64 `db:"key_id"`
			Claimed int           `db:"claimed"`
		}
		if e := a.db.Get(&back, `UPDATE denma_api_campaigns x SET state = 'draft', error = $2, claimed = 0
			FROM (SELECT campaign_id, claimed FROM denma_api_campaigns WHERE campaign_id = $1 FOR UPDATE) o
			WHERE x.campaign_id = o.campaign_id AND x.state = 'starting' RETURNING x.key_id, o.claimed`,
			id, err.Error()); e != nil && !errors.Is(e, sql.ErrNoRows) {
			a.log.Printf("denma: error recording API campaign %d's failure: %v", id, e)
		}
		if back.KeyID.Valid {
			a.apiUnclaim(int(back.KeyID.Int64), denmaAPIUseSend, back.Claimed)
		}
	}
	var x struct {
		Since  time.Time `db:"since"`
		ListID int       `db:"list_id"`
		KeyID  int       `db:"key_id"`
	}
	if err := a.db.Get(&x, `SELECT COALESCE(l.created_at, x.created_at) AS since, COALESCE(x.list_id, 0) AS list_id, x.key_id
		FROM denma_api_campaigns x LEFT JOIN lists l ON l.id = x.list_id
		WHERE x.campaign_id = $1 AND x.state = 'starting'`, id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			failed(err)
		}
		return // cancelled meanwhile
	}
	denmaImportCheck(a.db, a.ko)(x.Since, log.New(io.Discard, "", 0)) // cmd/denma_emailcheck.go

	// The check may have blocklisted every one of them.
	if n, err := a.apiListRecipients(x.ListID); err != nil || n == 0 {
		if err == nil {
			err = errors.New("No one can be sent this campaign: its recipients' domains can't receive mail, or they're blocklisted. Add others and start it again, or cancel it.")
		}
		failed(err)
		return
	}

	camp, err := a.core.GetCampaign(id, "", "")
	if err != nil {
		failed(err)
		return
	}
	status := models.CampaignStatusRunning
	if camp.SendAt.Valid && camp.SendAt.Time.After(time.Now()) {
		status = models.CampaignStatusScheduled
	}
	if err := a.denmaCheckCampaignStart(id, status); err != nil { // the sender's domain, verified (cmd/denma_domains.go)
		failed(err)
		return
	}
	// Started only if it wasn't cancelled meanwhile, nor its key deleted as
	// leaked (under its lock, cmd/denma_api.go).
	kl := a.apiKeyLock(x.KeyID)
	kl.Lock()
	defer kl.Unlock()
	if live, err := a.apiKeyLive(x.KeyID, true); err != nil || !live {
		if err == nil {
			err = errors.New("Its key was deleted as leaked.")
			if _, e := a.db.Exec(`UPDATE campaigns SET status = 'cancelled', updated_at = NOW() WHERE id = $1 AND status = 'draft'`, id); e != nil {
				a.log.Printf("denma: error cancelling API campaign %d: %v", id, e)
			}
		}
		failed(err)
		return
	}
	res, err := a.db.Exec(`UPDATE denma_api_campaigns SET state = 'started', error = '' WHERE campaign_id = $1 AND state = 'starting'`, id)
	if err != nil {
		failed(err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	if _, err := a.core.UpdateCampaignStatus(id, status); err != nil {
		_, _ = a.db.Exec(`UPDATE denma_api_campaigns SET state = 'starting' WHERE campaign_id = $1`, id)
		failed(err)
		return
	}
	a.log.Printf("denma: API campaign %d %s", id, status)
}

// DenmaAPIGetCampaign answers how a campaign stands.
func (a *App) DenmaAPIGetCampaign(c echo.Context) error {
	id, _ := strconv.Atoi(c.Param("id"))
	out, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaAPIAddRecipients adds recipients to a campaign made with "start":
// false, before it's started.
func (a *App) DenmaAPIAddRecipients(c echo.Context) error {
	id, _ := strconv.Atoi(c.Param("id"))
	var r struct {
		Recipients []denmaAPIRecipient `json:"recipients"`
	}
	if err := denmaAPIBind(c, &r); err != nil {
		return err
	}
	if err := a.apiCheckLinks(c, denmaAPIRecipientAttribs(r.Recipients)); err != nil {
		return err
	}
	if len(r.Recipients) > denmaAPIMaxRecipients {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("At most %d recipients a request.", denmaAPIMaxRecipients))
	}
	if _, err := a.apiCampaign(c, id); err != nil {
		return err
	}
	l := a.apiCampaignLock(id)
	l.Lock()
	defer l.Unlock()
	var x struct {
		State  string        `db:"state"`
		ListID sql.NullInt64 `db:"list_id"`
	}
	if err := a.db.Get(&x, `SELECT x.state, x.list_id FROM denma_api_campaigns x JOIN campaigns c ON c.id = x.campaign_id
		WHERE x.campaign_id = $1 AND c.status = 'draft'`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusConflict, "The campaign has started; recipients can only be added before.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if x.State != "draft" || !x.ListID.Valid {
		return echo.NewHTTPError(http.StatusConflict, "The campaign has started; recipients can only be added before.")
	}
	skipped, err := a.apiAddRecipients(c, int(x.ListID.Int64), r.Recipients)
	if err != nil {
		return err
	}
	out, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	out.Skipped = skipped
	a.apiLog(c, "campaign %d: recipients added (%d now, %d skipped)", id, out.Recipients, len(skipped))
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaAPICampaignLocks keep adding recipients to a draft and starting it
// apart (the process is the only one), so a start claims from the key's limit
// exactly the recipients it sends: none are added after it counts them.
var denmaAPICampaignLocks [64]sync.Mutex

func (a *App) apiCampaignLock(id int) *sync.Mutex {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s/%d", a.ko.String("denma.center"), id)
	return &denmaAPICampaignLocks[h.Sum32()%uint32(len(denmaAPICampaignLocks))]
}

// DenmaAPIStartCampaign starts a campaign made with "start": false.
func (a *App) DenmaAPIStartCampaign(c echo.Context) error {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := a.apiCanSend(); err != nil {
		return err
	}
	l := a.apiCampaignLock(id)
	l.Lock()
	defer l.Unlock()
	cur, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	if cur.Recipients == 0 && cur.Status == models.CampaignStatusDraft {
		return echo.NewHTTPError(http.StatusBadRequest,
			"No one can be sent this campaign: it has no recipients, or every one was skipped. Add recipients first, or cancel it.")
	}
	// Claim its recipients from the key's daily limit.
	if cur.Status == models.CampaignStatusDraft {
		if err := a.apiSendClaim(c, cur.Recipients); err != nil {
			return err
		}
	}
	res, err := a.db.Exec(`UPDATE denma_api_campaigns x SET state = 'starting', error = '', claimed = $2 FROM campaigns c
		WHERE c.id = x.campaign_id AND x.campaign_id = $1 AND x.state = 'draft' AND c.status = 'draft'`, id, cur.Recipients)
	if err != nil {
		a.apiUnclaim(apiKeyOf(c).ID, denmaAPIUseSend, cur.Recipients)
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if cur.Status == models.CampaignStatusDraft {
			a.apiUnclaim(apiKeyOf(c).ID, denmaAPIUseSend, cur.Recipients)
		}
		if cur.Status == models.CampaignStatusCancelled {
			return echo.NewHTTPError(http.StatusConflict, "The campaign is cancelled.")
		}
		return echo.NewHTTPError(http.StatusConflict, "The campaign has started already.")
	}
	go a.apiStart(id)
	out, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	var listID int
	if err := a.db.Get(&listID, `SELECT COALESCE(list_id, 0) FROM denma_api_campaigns WHERE campaign_id = $1`, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if out.Skipped, err = a.apiNotSent(listID, []denmaAPISkip{}); err != nil { // who it won't go to
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	a.apiLog(c, "campaign %d starting", id)
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaAPICancelCampaign cancels a campaign: before it starts, while
// scheduled, or while it's sending.
func (a *App) DenmaAPICancelCampaign(c echo.Context) error {
	id, _ := strconv.Atoi(c.Param("id"))
	cur, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	switch cur.Status {
	case models.CampaignStatusRunning, models.CampaignStatusPaused, models.CampaignStatusDraft, "starting", models.CampaignStatusScheduled:
		if err := a.apiCancelCampaign(id, cur.Status); err != nil {
			return err
		}
	default:
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("The campaign is %s already.", cur.Status))
	}
	out, err := a.apiCampaign(c, id)
	if err != nil {
		return err
	}
	a.apiLog(c, "campaign %d cancelled", id)
	return c.JSON(http.StatusOK, okResp{out})
}

// apiCancelCampaign cancels an API campaign in the given status.
func (a *App) apiCancelCampaign(id int, status string) error {
	switch status {
	case models.CampaignStatusRunning, models.CampaignStatusPaused:
		if _, err := a.core.UpdateCampaignStatus(id, models.CampaignStatusCancelled); err != nil {
			return err
		}
		a.manager.StopCampaign(id)
	default: // a draft (or starting), or scheduled
		if _, err := a.db.Exec(`UPDATE campaigns SET status = 'cancelled', updated_at = NOW() WHERE id = $1 AND status IN ('draft', 'scheduled')`, id); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if _, err := a.db.Exec(`UPDATE denma_api_campaigns SET state = 'started' WHERE campaign_id = $1`, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return nil
}

// ---- Where the links a key sends go ----

// reDenmaLink finds links in text, once its HTML entities and CSS escapes
// are decoded (denmaLinkText): http(s) addresses, with any slashes and
// backslashes or none (browsers read https:\\host and https:host as
// https://host), and protocol-relative ones (//host) in any attribute or CSS
// url().
var reDenmaLink = regexp.MustCompile(`(?i)\bhttps?:[/\\]*([^/\\\s"'<>()\[\]{}^|?#]+)|(?:=|url\()\s*["']?\s*[/\\]{2}([^/\\\s"'<>()]+)`)

// reDenmaCSSEscape is a CSS escape: a backslash and up to 6 hex digits.
var reDenmaCSSEscape = regexp.MustCompile(`\\([0-9a-fA-F]{1,6})\s?`)

// reDenmaAttrValue is a quoted attribute value or a CSS url(), where
// browsers drop a link's tabs and newlines (ht\ntps://evil.example).
var reDenmaAttrValue = regexp.MustCompile(`=\s*"[^"]*"|=\s*'[^']*'|(?i)url\([^)]*\)`)

// denmaDropTabNL is text without tabs and newlines.
func denmaDropTabNL(s string) string {
	return strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(s)
}

// denmaLinkText decodes what could hide a link from reDenmaLink: HTML
// entities (twice encoded too), CSS escapes, and tabs and newlines in
// attribute values.
func denmaLinkText(s string) string {
	for i := 0; i < 3 && strings.ContainsAny(s, "&\\"); i++ {
		d := html.UnescapeString(s)
		d = reDenmaCSSEscape.ReplaceAllStringFunc(d, func(m string) string {
			n, err := strconv.ParseInt(strings.TrimSpace(m[1:]), 16, 32)
			if err != nil || n < 0x20 || n > 0x10ffff { // not a control character: \\e in https:\\evil isn't one
				return m
			}
			return string(rune(n))
		})
		if d == s {
			break
		}
		s = d
	}
	return reDenmaAttrValue.ReplaceAllStringFunc(s, denmaDropTabNL)
}

// denmaTextLinkHosts returns the hosts of the links in a text.
func denmaTextLinkHosts(t string) []string {
	var out []string
	for _, m := range reDenmaLink.FindAllStringSubmatch(denmaLinkText(t), -1) {
		h := m[1] + m[2]
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:] // user@host
		}
		if h != "" {
			out = append(out, strings.ToLower(h))
		}
	}
	return out
}

// denmaNameHasLink reports whether a recipient's name has a link in it.
func denmaNameHasLink(name string) bool {
	return len(denmaTextLinkHosts(name)) > 0 || strings.Contains(strings.ToLower(denmaLinkText(name)), "www.")
}

// denmaLinkDomainsForm checks a key's link domains: domains (an address's
// host is taken), lowercase, each once.
func denmaLinkDomainsForm(in []string) ([]string, error) {
	out := []string{}
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s == "" {
			continue
		}
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			s = u.Hostname()
		}
		s = strings.TrimSuffix(strings.TrimPrefix(s, "*."), ".")
		if !reDenmaHostName.MatchString(s) {
			return nil, fmt.Errorf("%q isn't a domain, such as example.org.", s)
		}
		if !denmaHasString(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > denmaAPIMaxLinks {
		return nil, fmt.Errorf("Give at most %d link domains.", denmaAPIMaxLinks)
	}
	return out, nil
}

// apiOwnLinkDomains are the domains a key's links can always go to: the
// center's sending domains (or its sender's, if it has none yet), its
// website (in every campaign's footer), and the app's own (its uploads,
// archive and tracked links).
func (a *App) apiOwnLinkDomains() []string {
	out := []string{}
	if u, err := url.Parse(a.urlCfg.RootURL); err == nil && u.Hostname() != "" {
		out = append(out, strings.ToLower(u.Hostname()))
	}
	doms, err := a.denmaCenterDomains()
	if err != nil {
		a.log.Printf("denma: error reading the center's domains: %v", err)
	}
	for _, d := range doms {
		out = append(out, strings.ToLower(d.Domain))
	}
	if len(doms) == 0 {
		if d := denmaSenderDomain(a.cfg.FromEmail); d != "" {
			out = append(out, strings.ToLower(d))
		}
	}
	if h := denmaWebsiteHost(a.ko.String("denma.center")); h != "" && !slices.Contains(out, h) {
		out = append(out, h)
	}
	return out
}

// denmaLinkAllowed is whether host is one of domains or below one.
func denmaLinkAllowed(host string, domains []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// denmaLinkHosts returns the hosts of the links in values' text: strings,
// and the strings in maps and lists, at any depth.
func denmaLinkHosts(values ...any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			out = append(out, denmaTextLinkHosts(t)...)
			// A value that's one address may go in an href, where its tabs
			// and newlines are dropped.
			if u := strings.TrimSpace(denmaDropTabNL(t)); u != strings.TrimSpace(t) && !strings.ContainsAny(u, " ") {
				out = append(out, denmaTextLinkHosts(u)...)
			}
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case []map[string]any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	for _, v := range values {
		walk(v)
	}
	return out
}

// denmaAPIRecipientAttribs is recipients' attributes, for denmaLinkHosts.
func denmaAPIRecipientAttribs(rs []denmaAPIRecipient) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Attribs)
	}
	return out
}

// apiCheckLinks refuses a request whose text has a link (in its subject,
// body, data or attributes) to anywhere but the key's link domains and the
// center's own (apiOwnLinkDomains): a leaked key can't send the center's
// e-mails with someone else's links.
func (a *App) apiCheckLinks(c echo.Context, values ...any) error {
	hosts := denmaLinkHosts(values...)
	if len(hosts) == 0 {
		return nil
	}
	return denmaLinksAllowed(hosts, append(a.apiOwnLinkDomains(), apiKeyOf(c).LinkDomains...))
}

// denmaLinksAllowed refuses the first of hosts that isn't one of allowed.
func denmaLinksAllowed(hosts, allowed []string) error {
	for _, h := range hosts {
		if !denmaLinkAllowed(h, allowed) {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
				"A link goes to %s, which isn't one of this key's link domains or the center's (%s). The center's admins can add it to the key's link domains, on its Config -> API page.",
				h, strings.Join(allowed, ", ")))
		}
	}
	return nil
}

// denmaCheckRendered checks the links of an e-mail as it's filled in, which
// its template's logic (print, sprig's functions, a recipient's data) can
// make, as apiCheckLinks: the center's template's own (tpl) are allowed too.
func denmaCheckRendered(allowed []string, tpl string, texts ...string) error {
	allowed = append(append([]string{}, allowed...), denmaTextLinkHosts(tpl)...)
	for _, t := range texts {
		if err := denmaLinksAllowed(denmaTextLinkHosts(t), allowed); err != nil {
			return err
		}
	}
	return nil
}

// denmaAPICampLinks caches each API campaign's allowed link domains, by
// center and campaign ID; nil for a campaign that isn't one.
var denmaAPICampLinks sync.Map

// denmaCheckCampaignLinks checks an API campaign's e-mail, as it's sent, as
// denmaCheckRendered: one with a link elsewhere isn't sent, and is a failed
// send (cmd/denma_retries.go), so that a template's logic can't send the
// center's e-mails with someone else's links.
func denmaCheckCampaignLinks(slug string, m models.Message) error {
	if m.Campaign == nil || denmaHub == nil {
		return nil
	}
	key := slug + "/" + strconv.Itoa(m.Campaign.ID)
	v, ok := denmaAPICampLinks.Load(key)
	if !ok {
		ctr := denmaHub.get(slug)
		if ctr == nil {
			return nil
		}
		var doms pq.StringArray
		err := ctr.app.db.Get(&doms, `SELECT k.link_domains FROM denma_api_campaigns x JOIN denma_api_keys k ON k.id = x.key_id
			WHERE x.campaign_id = $1`, m.Campaign.ID)
		var allowed []string
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			allowed = append(ctr.app.apiOwnLinkDomains(), doms...)
		}
		v, _ = denmaAPICampLinks.LoadOrStore(key, allowed)
	}
	allowed := v.([]string)
	if allowed == nil {
		return nil
	}
	// And the center's website as it is now: the footer links it, and it
	// may have changed since the campaign started.
	if h := denmaWebsiteHost(slug); h != "" {
		allowed = append(slices.Clip(allowed), h)
	}
	// Where its tracked links go, which the template's logic can make
	// ({{ TrackLink (print "ht" "tps://...") . }}): the e-mail has only ours.
	var dests []string
	if ctr := denmaHub.get(slug); ctr != nil {
		var err error
		if dests, err = denmaTrackedURLs(ctr, string(m.Body), string(m.AltBody)); err != nil {
			return err
		}
	}
	if err := denmaCheckRendered(allowed, m.Campaign.TemplateBody, append([]string{m.Subject, string(m.Body), string(m.AltBody)}, dests...)...); err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return fmt.Errorf("not sent: %v", he.Message)
		}
		return err
	}
	return nil
}

// reDenmaTrackedLink is a tracked link's UUID in an e-mail (LinkTrackURL:
// .../link/<link>/<campaign>/<subscriber>).
var reDenmaTrackedLink = regexp.MustCompile(`/link/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/`)

// denmaTrackedURLCache is tracked links' destinations, by center and link
// UUID (a link's never changes).
var denmaTrackedURLCache sync.Map

// denmaTrackedURLs is where the tracked links in texts go, in center ctr,
// without tabs and newlines (which a browser drops when it follows one).
func denmaTrackedURLs(ctr *denmaCenter, texts ...string) ([]string, error) {
	var out, missing []string
	seen := map[string]bool{}
	for _, t := range texts {
		for _, m := range reDenmaTrackedLink.FindAllStringSubmatch(t, -1) {
			id := strings.ToLower(m[1])
			if seen[id] {
				continue
			}
			seen[id] = true
			if u, ok := denmaTrackedURLCache.Load(ctr.Slug + "/" + id); ok {
				out = append(out, u.(string))
			} else {
				missing = append(missing, id)
			}
		}
	}
	if len(missing) > 0 {
		var links []struct {
			UUID string `db:"uuid"`
			URL  string `db:"url"`
		}
		if err := ctr.app.db.Select(&links, `SELECT uuid::TEXT AS uuid, url FROM links WHERE uuid = ANY($1::UUID[])`, pq.Array(missing)); err != nil {
			return nil, err
		}
		for _, l := range links {
			u := denmaDropTabNL(l.URL)
			denmaTrackedURLCache.Store(ctr.Slug+"/"+l.UUID, u)
			out = append(out, u)
		}
	}
	return out, nil
}
