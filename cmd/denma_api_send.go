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
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/textproto"
	"sort"
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
	denmaAPIMaxTags       = 20 // tags a subscriber request adds

	denmaAPINotSent      = "not sent" // a skip's reason, unless the request's at fault
	denmaAPINoRecipients = "No one can be sent this campaign: it has no recipients, or every one was skipped (unsubscribed, bounced, or their domain can't receive mail). Nothing was made or sent."
)

// initDenmaAPIPublicHandlers registers the API, in a center.
func initDenmaAPIPublicHandlers(g *echo.Group, a *App) {
	if denmaHub == nil || a.ko.String("denma.center") == "" {
		return
	}
	g.GET("/v1/openapi.json", a.DenmaAPIOpenAPI) // no key (cmd/denma_api_openapi.go)
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
	{http.MethodPost, "/subscribers", denmaAPICanSubscribers, func(a *App) echo.HandlerFunc { return a.DenmaAPISubscribe }},
	{http.MethodGet, "/events", denmaAPICanEvents, func(a *App) echo.HandlerFunc { return a.DenmaAPIEvents }}, // cmd/denma_api_events.go
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
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("The request is over %d MB.", denmaAPIMaxBody>>20))
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
	c.Set(denmaAPIResultCtx, append(res, msg))
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
		sub, why, err := a.apiTxRecipient(email, name, skip)
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
		h := textproto.MIMEHeader{}
		h.Set(models.EmailHeaderSubscriberUUID, sub.UUID) // so its bounces and complaints find it (cmd/denma_bounces.go)
		if replyTo == "" {
			replyTo = a.denmaReplyTo() // the center's default (cmd/denma_reply_to.go)
		}
		if replyTo != "" {
			h.Set("Reply-To", replyTo)
		}
		msgs = append(msgs, models.Message{
			Subscriber:  sub,
			To:          []string{sub.Email},
			From:        from,
			Subject:     m.Subject,
			ContentType: m.ContentType,
			Messenger:   emailMsgr,
			Body:        m.Body,
			AltBody:     []byte(m.AltBody),
			Attachments: tpl.Attachments,
			Headers:     h,
		})
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
	claimed := false
	if cc != nil && len(msgs) > 0 {
		got, ok := cc.Claim(len(msgs))
		if got < len(msgs) {
			if ok {
				cc.Release(got)
			}
			a.apiUnclaim(k.ID, denmaAPIUseSend, len(msgs))
			return a.apiCapErr(len(msgs))
		}
		claimed = ok
	}
	for i, msg := range msgs {
		if claimed {
			msg.Headers.Set(denmaCenterClaimHeader, "1") // cmd/denma_daily.go
		}
		if err := a.manager.PushMessage(msg); err != nil {
			if claimed {
				cc.Release(1)
			}
			res.Skipped = append(res.Skipped, denmaAPISkip{subs[i].Email, "couldn't be sent: " + err.Error()})
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
// (added without lists if new, so that bounces and complaints are recorded),
// or denmaAPINotSent if it's not sent to.
func (a *App) apiTxRecipient(email, name string, skip map[string]bool) (models.Subscriber, string, error) {
	var sub models.Subscriber
	if _, ok := denmaSharedBlock(a.db, email); ok { // cmd/denma_blocklist.go
		return sub, denmaAPINotSent, nil
	}
	var kind string
	if err := a.db.Get(&kind, `SELECT COALESCE((SELECT kind FROM denma_optouts WHERE email_hash = denma_email_hash($1)), '')`, email); err != nil {
		return sub, "", err
	}
	if kind == denmaOptOutComplained {
		return sub, denmaAPINotSent, nil
	}
	if bad, reason := denmaEmailBad(email, skip); bad {
		denmaBlocklist(a.db, email, reason)
		denmaBlockEverywhere(email, reason, a.ko.String("denma.center"))
		return sub, denmaAPINotSent, nil
	}

	get := func() error {
		return a.db.Get(&sub, `SELECT id, uuid, email, name, attribs, status, created_at, updated_at FROM subscribers WHERE LOWER(email) = $1`, email)
	}
	err := get()
	if errors.Is(err, sql.ErrNoRows) {
		if name == "" {
			name = strings.Split(email, "@")[0]
		}
		if _, err := a.db.Exec(`INSERT INTO subscribers (uuid, email, name, status) VALUES (gen_random_uuid(), $1, $2, 'enabled')
			ON CONFLICT DO NOTHING`, email, name); err != nil {
			return sub, "", err
		}
		err = get()
	}
	if err != nil {
		return sub, "", err
	}
	if sub.Status == models.SubscriberStatusBlockListed {
		if reason, _ := sub.Attribs["auto_blocklist_reason"].(string); reason != "" {
			return sub, denmaAPINotSent, nil
		}
		// A hard bounce here, which the shared blocklist only has after the
		// minute's job (cmd/denma_blocklist.go).
		var bounced bool
		if err := a.db.Get(&bounced, `SELECT EXISTS (SELECT 1 FROM bounces WHERE subscriber_id = $1 AND type = 'hard')`, sub.ID); err != nil {
			return sub, "", err
		}
		if bounced {
			return sub, denmaAPINotSent, nil
		}
		// Unsubscribed (or blocklisted by an admin): transactional e-mail
		// still goes.
	}
	return sub, "", nil
}

// ---- POST /v1/campaigns ----

type denmaAPIRecipient struct {
	Email   string         `json:"email"`
	Name    string         `json:"name"`
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

	Skipped []denmaAPISkip `db:"-" json:"skipped,omitempty"` // when recipients are added
}

const denmaAPICampaignSQL = `SELECT c.id, c.uuid, c.name, c.subject, x.error,
	CASE WHEN x.state = 'starting' AND c.status = 'draft' THEN 'starting' ELSE c.status::TEXT END AS status,
	(SELECT COUNT(*) FROM subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id
		WHERE sl.list_id = x.list_id AND sl.status <> 'unsubscribed' AND s.status <> 'blocklisted') AS recipients,
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

	// Its list, and its recipients on it.
	var listID int
	if err := a.db.Get(&listID, `INSERT INTO lists (uuid, name, type, optin, tags, description)
		VALUES (gen_random_uuid(), $1, 'private', 'single', $2, $3) RETURNING id`,
		"API: "+name, pq.Array([]string{denmaAPIListTag}),
		fmt.Sprintf("The recipients of a campaign sent through the API (key %q). Deleted %d days after the campaign is done.", k.Name, denmaAPIListKeepDays)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	keep := false
	defer func() {
		if !keep {
			_, _ = a.db.Exec(`DELETE FROM lists WHERE id = $1`, listID)
		}
	}()
	skipped, err := a.apiAddRecipients(k.ID, listID, r.Recipients)
	if err != nil {
		return err
	}
	claimed := 0
	if start {
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
	if _, err := a.db.Exec(`INSERT INTO denma_api_campaigns (campaign_id, key_id, list_id, idempotency_key, state) VALUES ($1, $2, $3, $4, $5)`,
		camp.ID, k.ID, listID, idemArg, state); err != nil {
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
// subscribers, and returns those skipped (blocklisted). Their attributes are
// stored in the list's subscriber_lists.meta, for this campaign only
// (denmaAPIRecipientData); existing subscribers keep theirs, and their name
// unless it's a stand-in.
func (a *App) apiAddRecipients(keyID, listID int, in []denmaAPIRecipient) ([]denmaAPISkip, error) {
	skipped := []denmaAPISkip{}
	var emails, names, attribs []string
	seen := map[string]bool{}
	for _, r := range in {
		if _, ok := r.Attribs["tags"]; ok {
			return nil, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Recipient %s: %s", r.Email, denmaAPIAttribTagsMsg))
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
		emails = append(emails, email)
		names = append(names, denmaSignupField(map[string]string{"n": r.Name}, "n", 200))
		attribs = append(attribs, string(attr))
	}
	if len(emails) == 0 {
		return skipped, nil
	}

	fail := func(err error) ([]denmaAPISkip, error) {
		a.log.Printf("denma: error adding an API campaign's recipients: %v", err)
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "The recipients couldn't be added: "+err.Error())
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	// Existing subscribers: only a stand-in name is replaced (denmaAPIName).
	if _, err := tx.Exec(`UPDATE subscribers s SET name = i.name, updated_at = NOW()
		FROM unnest($1::TEXT[], $2::TEXT[]) AS i(email, name)
		WHERE LOWER(s.email) = i.email AND i.name <> ''
		AND LOWER(TRIM(s.name)) IN ('', i.email, split_part(i.email, '@', 1))`, pq.Array(emails), pq.Array(names)); err != nil {
		return fail(err)
	}
	// New ones (an address that opted out is added blocklisted).
	if _, err := tx.Exec(`INSERT INTO subscribers (uuid, email, name, status)
		SELECT gen_random_uuid(), i.email, COALESCE(NULLIF(i.name, ''), split_part(i.email, '@', 1)), 'enabled'
		FROM unnest($1::TEXT[], $2::TEXT[]) AS i(email, name)
		ON CONFLICT DO NOTHING`, pq.Array(emails), pq.Array(names)); err != nil {
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
		if s.Status != models.SubscriberStatusBlockListed {
			ids = append(ids, s.ID)
			continue
		}
		skipped = append(skipped, denmaAPISkip{s.Email, denmaAPINotSent})
	}
	// On the list, with their attributes; the latest if given again.
	if _, err := tx.Exec(`INSERT INTO subscriber_lists (subscriber_id, list_id, status, meta)
		SELECT s.id, $2, 'confirmed', CASE WHEN i.attribs = '{}'::JSONB THEN '{}'::JSONB ELSE jsonb_build_object('attribs', i.attribs) END
		FROM unnest($3::TEXT[], $4::JSONB[]) AS i(email, attribs) JOIN subscribers s ON LOWER(s.email) = i.email
		WHERE s.id = ANY($1::INT[])
		ON CONFLICT (subscriber_id, list_id) DO UPDATE SET meta = EXCLUDED.meta, updated_at = NOW()`,
		pq.Array(ids), listID, pq.Array(emails), pq.Array(attribs)); err != nil {
		return fail(err)
	}
	if err := denmaAPIContacts(tx, keyID, emails); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return skipped, nil
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
		if _, e := a.db.Exec(`UPDATE denma_api_campaigns SET state = 'draft', error = $2 WHERE campaign_id = $1 AND state = 'starting'`,
			id, err.Error()); e != nil {
			a.log.Printf("denma: error recording API campaign %d's failure: %v", id, e)
		}
	}
	var x struct {
		Since  time.Time `db:"since"`
		ListID int       `db:"list_id"`
	}
	if err := a.db.Get(&x, `SELECT COALESCE(l.created_at, x.created_at) AS since, COALESCE(x.list_id, 0) AS list_id
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
	// Started only if it wasn't cancelled meanwhile.
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
	if len(r.Recipients) > denmaAPIMaxRecipients {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("At most %d recipients a request.", denmaAPIMaxRecipients))
	}
	if _, err := a.apiCampaign(c, id); err != nil {
		return err
	}
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
	skipped, err := a.apiAddRecipients(apiKeyOf(c).ID, int(x.ListID.Int64), r.Recipients)
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

// DenmaAPIStartCampaign starts a campaign made with "start": false.
func (a *App) DenmaAPIStartCampaign(c echo.Context) error {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := a.apiCanSend(); err != nil {
		return err
	}
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
	res, err := a.db.Exec(`UPDATE denma_api_campaigns x SET state = 'starting', error = '' FROM campaigns c
		WHERE c.id = x.campaign_id AND x.campaign_id = $1 AND x.state = 'draft' AND c.status = 'draft'`, id)
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

// ---- POST /v1/subscribers ----

type denmaAPISubscriberReq struct {
	Email     string         `json:"email"`
	Name      string         `json:"name"`
	Lists     []int          `json:"lists"`
	Attribs   map[string]any `json:"attribs"`
	Tags      []string       `json:"tags"`
	Confirmed bool           `json:"confirmed"`
}

const denmaAPIAttribTagsMsg = `"tags" can't be in "attribs". Give them in POST /v1/subscribers' own "tags", which adds to the subscriber's.`

// DenmaAPISubscribe adds someone to some of the key's lists. With
// "confirmed" (and a consent record, denmaAPIConsentAt) they're subscribed
// at once; otherwise double opt-in lists send the confirmation e-mail.
//
// Someone who unsubscribed from the center is re-subscribed at once with
// "confirmed" and a consent dated after the opt-out, up to
// denmaAPIResubPerDay a day per key; otherwise they're sent the
// re-subscription confirmation (cmd/denma_resubscribe.go). Complaints,
// bounces and an older consent leave them alone.
//
// What's sent is added to the subscriber, never replacing anything
// (apiAppendSubscriber). New tags are added to the center's.
func (a *App) DenmaAPISubscribe(c echo.Context) error {
	var r denmaAPISubscriberReq
	if err := denmaAPIBind(c, &r); err != nil {
		return err
	}
	k := apiKeyOf(c)
	if len(k.ListIDs) == 0 {
		return echo.NewHTTPError(http.StatusForbidden, "This key has no lists to add subscribers to; choose them on the center's Config -> API page.")
	}
	email, err := a.importer.SanitizeEmail(strings.TrimSpace(r.Email))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, `"email" isn't a valid address.`)
	}
	email = strings.ToLower(email)
	listIDs := dedupInts(r.Lists)
	if len(listIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, `Give "lists": the IDs of the lists to add them to.`)
	}
	for _, id := range listIDs {
		allowed := false
		for _, l := range k.ListIDs {
			allowed = allowed || int(l) == id
		}
		if !allowed {
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("This key can't add subscribers to list %d.", id))
		}
	}
	name := denmaSignupField(map[string]string{"n": r.Name}, "n", 200)
	if _, ok := r.Attribs["tags"]; ok {
		return echo.NewHTTPError(http.StatusBadRequest, denmaAPIAttribTagsMsg)
	}
	if len(r.Attribs) > 0 {
		if b, _ := json.Marshal(r.Attribs); len(b) > denmaAPIMaxAttribs {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"attribs" is over %d bytes.`, denmaAPIMaxAttribs))
		}
	}
	if len(r.Tags) > denmaAPIMaxTags {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"tags" can have at most %d tags.`, denmaAPIMaxTags))
	}
	tags := denmaNormTags(r.Tags)
	var consentAt time.Time
	if r.Confirmed {
		if consentAt, err = denmaAPIConsentAt(r.Attribs); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}

	type result struct {
		Result       string   `json:"result"`
		Added        []int    `json:"added"`        // lists they're on now (confirmed or waiting to confirm)
		Resubscribed []int    `json:"resubscribed"` // of those, the ones they'd unsubscribed from
		TagsCreated  []string `json:"tags_created"` // new to the center: added to its tags
		AttribsKept  []string `json:"attribs_kept"` // attributes they already had another value for, kept
	}
	out := result{Added: []int{}, Resubscribed: []int{}, TagsCreated: []string{}, AttribsKept: []string{}}
	done := func(why string) error {
		if why != "" {
			a.apiLog(c, "subscriber: %s (%s)", out.Result, why) // the reason for the log, not the answer
		} else {
			a.apiLog(c, "subscriber: %s", out.Result)
		}
		if err := denmaAPIContacts(a.db, k.ID, []string{email}); err != nil { // its events (cmd/denma_api_events.go)
			a.log.Printf("denma: error recording API key %d's addresses: %v", k.ID, err)
		}
		return c.JSON(http.StatusOK, okResp{out})
	}

	// Them, added if they're new. An address that opted out before is added
	// blocklisted (cmd/denma_optouts.go).
	newName := name
	if newName == "" {
		newName = strings.Split(email, "@")[0]
	}
	var newID int
	err = a.db.Get(&newID, `INSERT INTO subscribers (uuid, email, name, status) VALUES (gen_random_uuid(), $1, $2, 'enabled')
		ON CONFLICT DO NOTHING RETURNING id`, email, newName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var sub models.Subscriber
	if err := a.db.Get(&sub, `SELECT id, uuid, email, name, attribs, status, created_at, updated_at FROM subscribers WHERE LOWER(email) = $1`, email); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// Blocklisted: a complaint, a bounce, or a consent from before they
	// unsubscribed.
	blocked := sub.Status == models.SubscriberStatusBlockListed
	if blocked {
		why, err := a.denmaResubRefused(sub)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if why != "" {
			out.Result = denmaAPINotSubscribed
			return done(why)
		}
		if r.Confirmed {
			var optedOut sql.NullTime
			if err := a.db.Get(&optedOut, `SELECT MAX(created_at) FROM denma_optouts WHERE email_hash = denma_email_hash($1)`, email); err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if optedOut.Valid && !consentAt.After(optedOut.Time) {
				out.Result = denmaAPINotSubscribed
				return done("consent from before they unsubscribed")
			}
		}
	}

	if out.TagsCreated, out.AttribsKept, err = a.apiAppendSubscriber(sub.ID, email, name, r.Attribs, tags); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(out.TagsCreated) > 0 {
		a.apiLog(c, "tags added to the center: %s", strings.Join(out.TagsCreated, ", "))
	}

	// Unsubscribed from the center: re-subscribed now, or on confirming.
	if blocked {
		now := false
		if r.Confirmed {
			ok, _, err := a.apiClaim(k.ID, denmaAPIUseResub, 1, denmaAPIResubPerDay)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if !ok {
				a.apiAlert(k.ID, k.Name, denmaAPIUseResub, fmt.Sprintf("re-subscribed %d people in a day", denmaAPIResubPerDay),
					fmt.Sprintf("It re-subscribes people who had unsubscribed, saying they've agreed again. It did that for %d people in the last 24 hours, "+
						"the most it can; others since are sent the e-mail to confirm it themselves. If its system shouldn't re-subscribe that many, "+
						"the key may have leaked: delete it as leaked.", denmaAPIResubPerDay))
			}
			now = ok
		}
		if !now {
			status, err := a.denmaResubscribe(sub, listIDs, nil)
			var he *echo.HTTPError
			switch {
			case errors.As(err, &he) && he.Code < 500:
				out.Result = denmaAPINotSubscribed
				return done(fmt.Sprint(he.Message))
			case err != nil:
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			out.Result = denmaAPIConfirmationSent
			return done(status)
		}
		if err := a.db.Select(&out.Resubscribed, `SELECT list_id FROM subscriber_lists
			WHERE subscriber_id = $1 AND list_id = ANY($2) AND status = 'unsubscribed' ORDER BY list_id`, sub.ID, pq.Array(listIDs)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		ids := make(pq.Int64Array, len(listIDs))
		for i, id := range listIDs {
			ids[i] = int64(id)
		}
		lists, err := a.denmaListsByID(ids)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if err := a.denmaResubConfirm(sub.ID, lists, nil, models.JSON{"resubscribed": true, "via": "API key: " + k.Name}); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		out.Added = listIDs
		out.Result = denmaAPIResubscribed
		return done("")
	}

	// The lists: confirmed if single opt-in or "confirmed". Lists they'd
	// unsubscribed from are as for anyone new.
	var rows []struct {
		ListID int            `db:"list_id"`
		Status string         `db:"status"`
		Was    sql.NullString `db:"was"`
	}
	if err := a.db.Select(&rows, `WITH was AS (
			SELECT list_id, status::TEXT AS status FROM subscriber_lists WHERE subscriber_id = $1 AND list_id = ANY($2)
		)
		INSERT INTO subscriber_lists (subscriber_id, list_id, status)
		SELECT $1, l.id, CASE WHEN l.optin = 'single' OR $3 THEN 'confirmed'::subscription_status ELSE 'unconfirmed'::subscription_status END
		FROM lists l WHERE l.id = ANY($2)
		ON CONFLICT (subscriber_id, list_id) DO UPDATE SET status = CASE
			WHEN subscriber_lists.status = 'confirmed' THEN subscriber_lists.status
			ELSE EXCLUDED.status END, updated_at = NOW()
		RETURNING list_id, status::TEXT AS status, (SELECT status FROM was WHERE was.list_id = subscriber_lists.list_id) AS was`,
		sub.ID, pq.Array(listIDs), r.Confirmed); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var unconfirmed []int
	for _, row := range rows {
		if row.Status == models.SubscriptionStatusUnsubscribed {
			continue // kept so by the database (cmd/denma_optouts.go)
		}
		out.Added = append(out.Added, row.ListID)
		if row.Was.String == models.SubscriptionStatusUnsubscribed {
			out.Resubscribed = append(out.Resubscribed, row.ListID)
		}
		if row.Status == models.SubscriptionStatusUnconfirmed {
			unconfirmed = append(unconfirmed, row.ListID)
		}
	}
	switch {
	case len(out.Added) == 0:
		out.Result = denmaAPINotSubscribed
		return done("unsubscribed from the lists")
	case len(unconfirmed) > 0:
		// The confirmation e-mail, with the opt-in checks (cmd/denma_emailcheck.go).
		if _, err := a.fnOptinNotify(sub, unconfirmed); err != nil {
			var he *echo.HTTPError
			if errors.As(err, &he) && he.Code < 500 {
				out.Result = denmaAPIConfirmationNotSent
				return done(fmt.Sprint(he.Message))
			}
			return err
		}
		out.Result = denmaAPIConfirmationSent
	case len(out.Resubscribed) > 0:
		out.Result = denmaAPIResubscribed
	default:
		out.Result = denmaAPISubscribed
	}
	return done("")
}

// What became of someone given to POST /v1/subscribers.
const (
	denmaAPISubscribed          = "subscribed"
	denmaAPIResubscribed        = "re-subscribed"
	denmaAPIConfirmationSent    = "confirmation sent"
	denmaAPIConfirmationNotSent = "added; confirmation not sent"
	denmaAPINotSubscribed       = "not subscribed"
)

// denmaAPIConsentSkew is how far in the future a consent's time may be (the
// other system's clock).
const denmaAPIConsentSkew = 5 * time.Minute

// denmaAPIConsentAt returns the latest "at" of attribs.consents, whose
// entries each need "at" (RFC 3339) and "source".
func denmaAPIConsentAt(attribs map[string]any) (time.Time, error) {
	const need = `"confirmed" needs a consent record: "attribs": {"consents": [{"at": "<when they agreed, RFC 3339>", "source": "<how>"}]}.`
	list, _ := attribs["consents"].([]any)
	var at time.Time
	for _, e := range list {
		m, _ := e.(map[string]any)
		s, _ := m["at"].(string)
		src, _ := m["source"].(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || strings.TrimSpace(src) == "" {
			return time.Time{}, errors.New(need + ` Each needs "at" and "source".`)
		}
		if t.After(time.Now().Add(denmaAPIConsentSkew)) {
			return time.Time{}, fmt.Errorf(`A consent's "at" (%s) is in the future.`, s)
		}
		if t.After(at) {
			at = t
		}
	}
	if at.IsZero() {
		return time.Time{}, errors.New(need)
	}
	return at, nil
}

// apiAppendSubscriber adds a request's name, attributes and tags to a
// subscriber's, with their row locked, so that two requests at once both
// count. The name replaces theirs only if it's a stand-in (denmaAPIName);
// the attributes are added to theirs (denmaAppendJSON), and the tags too,
// the center's tags getting those it doesn't have. It returns the tags new
// to the center and the attributes kept (they already had another value).
func (a *App) apiAppendSubscriber(id int, email, name string, attribs map[string]any, tags []string) (created, kept []string, err error) {
	created, kept = []string{}, []string{}
	tx, err := a.db.Beginx()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var cur struct {
		Name    string `db:"name"`
		Attribs []byte `db:"attribs"`
	}
	if err := tx.Get(&cur, `SELECT name, attribs FROM subscribers WHERE id = $1 FOR UPDATE`, id); err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(cur.Attribs, &m); err != nil || m == nil {
		m = map[string]any{} // not an object: listmonk's own handling starts afresh too
	}
	m = denmaAppendJSON(m, map[string]any(attribs), "", &kept).(map[string]any)
	if len(tags) > 0 {
		if err := tx.Select(&created, `INSERT INTO denma_tags (tag) SELECT UNNEST($1::TEXT[])
			ON CONFLICT DO NOTHING RETURNING tag`, pq.Array(tags)); err != nil {
			return nil, nil, err
		}
		add := make([]any, len(tags))
		for i, t := range tags {
			add[i] = t
		}
		have, _ := m["tags"].([]any) // an array, kept so by the database (denma_norm_tags)
		m["tags"] = denmaAppendJSON(have, add, "tags", &kept)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(`UPDATE subscribers SET name = $2, attribs = $3, updated_at = NOW() WHERE id = $1`,
		id, denmaAPIName(cur.Name, name, email), string(b)); err != nil {
		return nil, nil, err
	}
	sort.Strings(created)
	return created, kept, tx.Commit()
}

// denmaAPIName is a subscriber's name after a request gives one: theirs,
// unless it's a stand-in (empty, their address or the part before its @).
func denmaAPIName(cur, given, email string) string {
	local, _, _ := strings.Cut(email, "@")
	switch c := strings.ToLower(strings.TrimSpace(cur)); {
	case given == "":
		return cur
	case c == "", c == strings.ToLower(email), c == strings.ToLower(local):
		return given
	}
	return cur
}

// denmaAppendJSON adds add to old (decoded JSON) without taking anything
// away: what old doesn't have is added, objects are merged a key at a time,
// arrays get the items they don't have yet, and a value old already has is
// kept. Each place where add has another value, which is left out, is named
// in kept ("a.b").
func denmaAppendJSON(old, add any, path string, kept *[]string) any {
	if add == nil {
		return old
	}
	if old == nil {
		return add
	}
	switch o := old.(type) {
	case map[string]any:
		if ad, ok := add.(map[string]any); ok {
			out := make(map[string]any, len(o)+len(ad))
			for k, v := range o {
				out[k] = v
			}
			keys := make([]string, 0, len(ad))
			for k, v := range ad {
				if v != nil { // null adds nothing
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				out[k] = denmaAppendJSON(o[k], ad[k], p, kept)
			}
			return out
		}
	case []any:
		if ad, ok := add.([]any); ok {
			out := append([]any{}, o...)
			for _, v := range ad {
				if !denmaJSONHas(out, v) {
					out = append(out, v)
				}
			}
			return out
		}
	}
	if !denmaJSONEqual(old, add) {
		*kept = append(*kept, path)
	}
	return old
}

func denmaJSONHas(list []any, v any) bool {
	for _, e := range list {
		if denmaJSONEqual(e, v) {
			return true
		}
	}
	return false
}

// denmaJSONEqual compares decoded JSON values (encoding sorts objects' keys).
func denmaJSONEqual(x, y any) bool {
	a, err1 := json.Marshal(x)
	b, err2 := json.Marshal(y)
	return err1 == nil && err2 == nil && string(a) == string(b)
}
