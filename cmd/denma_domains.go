package main

// denma: the hub's Sending domains page (superadmins). Every center sends
// through the hub's mail server, Amazon SES, which sends only from domains it
// has verified. A superadmin adds a center's domain here for the center: it's
// set up in SES (Easy DKIM, a bounce subdomain as its custom MAIL FROM, the
// hub's configuration set and bounce and complaint notifications), and its DNS
// records are listed to send to whoever manages the center's DNS. SES and the
// records are checked in the background until it's verified, then daily.
//
// A center's senders (its own, its campaigns' and automations') must be on
// one of its domains, and campaigns start and tests send only once SES has
// verified it. Senders that were set before (DDL's, at the cutover) are added
// for their centers when the hub starts, and a domain can't be taken from a
// center whose sender is on it.
//
// SES is the hub's SMTP server's region (email-smtp.<region>.amazonaws.com),
// reached with the server's AWS credentials (its instance role). denma.ses is
// "fake" in dev, for an SES of our own (denmaFakeSES), or "off".
//
// Bounce and complaint notifications are set with SES's v1 API, the only one
// that has them; listmonk's webhook doesn't count complaints from the v2
// event destinations (internal/bounce/webhooks/ses.go).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	sestypes "github.com/aws/aws-sdk-go-v2/service/ses/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sesv2types "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaSPFValue   = "v=spf1 include:amazonses.com ~all"
	denmaDMARCValue = "v=DMARC1; p=none;"
)

// The hub's SMTP server, if it's SES's. Domains are checked with
// reDenmaDomain (cmd/denma_features.go).
var reDenmaSESHost = regexp.MustCompile(`^email-smtp(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com$`)

// denmaInitDomains creates the sending domains and the centers each is for
// (with the registry).
func denmaInitDomains(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS denma.sending_domains (
			domain     TEXT PRIMARY KEY CHECK (domain = LOWER(domain)),
			mail_from  TEXT NOT NULL,               -- its bounce (custom MAIL FROM) subdomain
			state      JSONB NOT NULL DEFAULT '{}', -- the last check (denmaDomainState)
			checked_at TIMESTAMP WITH TIME ZONE NULL,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS denma.sending_domain_centers (
			domain    TEXT NOT NULL REFERENCES denma.sending_domains(domain) ON DELETE CASCADE,
			center_id INTEGER NOT NULL REFERENCES denma.centers(id) ON DELETE CASCADE,
			PRIMARY KEY (domain, center_id)
		);`)
	return err
}

// denmaSESIdentity is a domain as SES has it.
type denmaSESIdentity struct {
	Exists         bool     `json:"exists"`
	Verified       bool     `json:"verified"`     // for sending
	Verification   string   `json:"verification"` // PENDING, SUCCESS, FAILED, TEMPORARY_FAILURE, NOT_STARTED
	VerifyError    string   `json:"verify_error"` // why it isn't, e.g. TYPE_NOT_FOUND
	DKIM           string   `json:"dkim"`         // the same values
	DKIMOrigin     string   `json:"dkim_origin"`  // AWS_SES (Easy DKIM), EXTERNAL, ...
	Tokens         []string `json:"tokens"`
	SigningZone    string   `json:"signing_zone"`
	MailFrom       string   `json:"mail_from"`
	MailFromStatus string   `json:"mail_from_status"` // PENDING, SUCCESS, FAILED, TEMPORARY_FAILURE
	MailFromMX     string   `json:"mail_from_mx"`     // USE_DEFAULT_VALUE or REJECT_MESSAGE
	ConfigSet      string   `json:"config_set"`
	BounceTopic    string   `json:"bounce_topic"`
	ComplaintTopic string   `json:"complaint_topic"`
	Headers        bool     `json:"headers"` // in both
}

// denmaSES is what the hub does in SES.
type denmaSES interface {
	Region() string
	// Get is the domain as SES has it; Exists is false if it hasn't.
	Get(ctx context.Context, domain string) (denmaSESIdentity, error)
	Create(ctx context.Context, domain, configSet string) error
	SetMailFrom(ctx context.Context, domain, mailFrom, behavior string) error
	RestartDKIM(ctx context.Context, domain string) error
	SetConfigSet(ctx context.Context, domain, set string) error
	// SetNotifications sends its bounces and complaints, with their
	// headers, to an SNS topic.
	SetNotifications(ctx context.Context, domain, topic string) error
	Delete(ctx context.Context, domain string) error
}

var denmaSESClients struct {
	sync.Mutex
	real map[string]*denmaRealSES
	fake *denmaFakeSES
}

// denmaSESFor is the hub's SES (its settings, ko), or nil and why not.
func denmaSESFor(ko *koanf.Koanf, db *sqlx.DB) (denmaSES, string) {
	mode := ko.String("denma.ses")
	if mode == "off" {
		return nil, "Sending domains are off (denma.ses)."
	}
	region := ""
	for _, s := range ko.Slices("smtp") {
		if !s.Bool("enabled") {
			continue
		}
		if m := reDenmaSESHost.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s.String("host")))); m != nil {
			region = m[1]
			break
		}
	}

	c := &denmaSESClients
	c.Lock()
	defer c.Unlock()
	if mode == "fake" {
		if region == "" {
			region = "ca-central-1"
		}
		if c.fake == nil {
			c.fake = &denmaFakeSES{region: region, db: db}
		}
		return c.fake, ""
	}
	if region == "" {
		return nil, "The hub's mail server (Settings -> SMTP) isn't Amazon SES, so there are no domains to set up."
	}
	if s, ok := c.real[region]; ok {
		return s, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Sprintf("Amazon SES (%s) can't be reached: %v", region, err)
	}
	if c.real == nil {
		c.real = map[string]*denmaRealSES{}
	}
	s := &denmaRealSES{region: region, v2: sesv2.NewFromConfig(cfg), v1: ses.NewFromConfig(cfg)}
	c.real[region] = s
	return s, ""
}

// ses is the hub's SES, or nil and why not.
func (d *denmaCenters) ses() (denmaSES, string) {
	base := d.current()
	return denmaSESFor(base.ko, base.db)
}

// denmaDomainsOn reports whether senders are checked against the sending
// domains: in multi-center mode, with SES.
func denmaDomainsOn() bool {
	if denmaHub == nil {
		return false
	}
	s, _ := denmaHub.ses()
	return s != nil
}

// denmaRealSES is Amazon SES.
type denmaRealSES struct {
	region string
	v2     *sesv2.Client
	v1     *ses.Client
}

func (s *denmaRealSES) Region() string { return s.region }

func (s *denmaRealSES) Get(ctx context.Context, domain string) (denmaSESIdentity, error) {
	out, err := s.v2.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(domain)})
	var nf *sesv2types.NotFoundException
	if errors.As(err, &nf) {
		return denmaSESIdentity{}, nil
	}
	if err != nil {
		return denmaSESIdentity{}, err
	}
	id := denmaSESIdentity{Exists: true, Verified: out.VerifiedForSendingStatus,
		Verification: string(out.VerificationStatus), ConfigSet: aws.ToString(out.ConfigurationSetName)}
	if v := out.VerificationInfo; v != nil {
		id.VerifyError = string(v.ErrorType)
	}
	if k := out.DkimAttributes; k != nil {
		id.DKIM, id.DKIMOrigin, id.Tokens, id.SigningZone = string(k.Status), string(k.SigningAttributesOrigin), k.Tokens, aws.ToString(k.SigningHostedZone)
	}
	if m := out.MailFromAttributes; m != nil {
		id.MailFrom, id.MailFromStatus, id.MailFromMX = aws.ToString(m.MailFromDomain), string(m.MailFromDomainStatus), string(m.BehaviorOnMxFailure)
	}
	n, err := s.v1.GetIdentityNotificationAttributes(ctx, &ses.GetIdentityNotificationAttributesInput{Identities: []string{domain}})
	if err != nil {
		return id, err
	}
	if a, ok := n.NotificationAttributes[domain]; ok {
		id.BounceTopic, id.ComplaintTopic = aws.ToString(a.BounceTopic), aws.ToString(a.ComplaintTopic)
		id.Headers = a.HeadersInBounceNotificationsEnabled && a.HeadersInComplaintNotificationsEnabled
	}
	return id, nil
}

func (s *denmaRealSES) Create(ctx context.Context, domain, configSet string) error {
	in := &sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String(domain)}
	if configSet != "" {
		in.ConfigurationSetName = aws.String(configSet)
	}
	_, err := s.v2.CreateEmailIdentity(ctx, in)
	var ae *sesv2types.AlreadyExistsException
	if errors.As(err, &ae) {
		return nil
	}
	return err
}

func (s *denmaRealSES) SetMailFrom(ctx context.Context, domain, mailFrom, behavior string) error {
	_, err := s.v2.PutEmailIdentityMailFromAttributes(ctx, &sesv2.PutEmailIdentityMailFromAttributesInput{
		EmailIdentity: aws.String(domain), MailFromDomain: aws.String(mailFrom),
		BehaviorOnMxFailure: sesv2types.BehaviorOnMxFailure(behavior)})
	return err
}

func (s *denmaRealSES) RestartDKIM(ctx context.Context, domain string) error {
	_, err := s.v2.PutEmailIdentityDkimSigningAttributes(ctx, &sesv2.PutEmailIdentityDkimSigningAttributesInput{
		EmailIdentity: aws.String(domain), SigningAttributesOrigin: sesv2types.DkimSigningAttributesOriginAwsSes})
	return err
}

func (s *denmaRealSES) SetConfigSet(ctx context.Context, domain, set string) error {
	_, err := s.v2.PutEmailIdentityConfigurationSetAttributes(ctx, &sesv2.PutEmailIdentityConfigurationSetAttributesInput{
		EmailIdentity: aws.String(domain), ConfigurationSetName: aws.String(set)})
	return err
}

// SetNotifications spaces its calls a second apart: SES allows one a second.
func (s *denmaRealSES) SetNotifications(ctx context.Context, domain, topic string) error {
	wait := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	}
	for _, t := range []sestypes.NotificationType{sestypes.NotificationTypeBounce, sestypes.NotificationTypeComplaint} {
		if _, err := s.v1.SetIdentityNotificationTopic(ctx, &ses.SetIdentityNotificationTopicInput{
			Identity: aws.String(domain), NotificationType: t, SnsTopic: aws.String(topic)}); err != nil {
			return err
		}
		if err := wait(); err != nil {
			return err
		}
		if _, err := s.v1.SetIdentityHeadersInNotificationsEnabled(ctx, &ses.SetIdentityHeadersInNotificationsEnabledInput{
			Identity: aws.String(domain), NotificationType: t, Enabled: true}); err != nil {
			return err
		}
		if err := wait(); err != nil {
			return err
		}
	}
	return nil
}

func (s *denmaRealSES) Delete(ctx context.Context, domain string) error {
	_, err := s.v2.DeleteEmailIdentity(ctx, &sesv2.DeleteEmailIdentityInput{EmailIdentity: aws.String(domain)})
	var nf *sesv2types.NotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// denmaFakeSES is an SES of our own, for dev (denma.ses = fake), kept in the
// database. A domain is verified 30 seconds after it's added, or its DKIM
// fails if its name has "fail" in it, or it stays pending with "pending".
type denmaFakeSES struct {
	region string
	db     *sqlx.DB
	once   sync.Once
}

type denmaFakeIdentity struct {
	denmaSESIdentity
	Since time.Time `json:"since"`
}

func (s *denmaFakeSES) Region() string { return s.region }

func (s *denmaFakeSES) load(domain string) (*denmaFakeIdentity, error) {
	s.once.Do(func() {
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS denma.dev_ses_identities (domain TEXT PRIMARY KEY, data JSONB NOT NULL)`); err != nil {
			lo.Printf("denma: error creating the fake SES's table: %v", err)
		}
	})
	var b []byte
	err := s.db.Get(&b, `SELECT data FROM denma.dev_ses_identities WHERE domain = $1`, domain)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id denmaFakeIdentity
	return &id, json.Unmarshal(b, &id)
}

func (s *denmaFakeSES) save(domain string, id *denmaFakeIdentity) error {
	b, _ := json.Marshal(id)
	_, err := s.db.Exec(`INSERT INTO denma.dev_ses_identities (domain, data) VALUES ($1, $2)
		ON CONFLICT (domain) DO UPDATE SET data = EXCLUDED.data`, domain, b)
	return err
}

func (s *denmaFakeSES) edit(domain string, f func(*denmaFakeIdentity)) error {
	id, err := s.load(domain)
	if err != nil {
		return err
	}
	if id == nil {
		return fmt.Errorf("NotFoundException: %s isn't in SES", domain)
	}
	f(id)
	return s.save(domain, id)
}

func denmaFakeTokens() []string {
	const abc = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]string, 3)
	for i := range out {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		for j := range b {
			b[j] = abc[int(b[j])%len(abc)]
		}
		out[i] = string(b)
	}
	return out
}

func (s *denmaFakeSES) Get(_ context.Context, domain string) (denmaSESIdentity, error) {
	id, err := s.load(domain)
	if err != nil || id == nil {
		return denmaSESIdentity{}, err
	}
	if time.Since(id.Since) > 30*time.Second && id.DKIM == "PENDING" {
		switch {
		case strings.Contains(domain, "pending"):
		case strings.Contains(domain, "fail"):
			id.DKIM, id.Verification, id.VerifyError = "FAILED", "FAILED", "TYPE_NOT_FOUND"
		default:
			id.DKIM, id.Verification, id.VerifyError, id.Verified = "SUCCESS", "SUCCESS", "", true
		}
	}
	if id.MailFrom != "" && id.MailFromStatus == "PENDING" && time.Since(id.Since) > 30*time.Second && !strings.Contains(domain, "pending") {
		id.MailFromStatus = "SUCCESS"
	}
	return id.denmaSESIdentity, nil
}

func (s *denmaFakeSES) Create(_ context.Context, domain, configSet string) error {
	if id, err := s.load(domain); err != nil || id != nil {
		return err
	}
	return s.save(domain, &denmaFakeIdentity{Since: time.Now(), denmaSESIdentity: denmaSESIdentity{Exists: true,
		Verification: "PENDING", DKIM: "PENDING", DKIMOrigin: "AWS_SES", Tokens: denmaFakeTokens(),
		SigningZone: "dkim.amazonses.com", ConfigSet: configSet}})
}

func (s *denmaFakeSES) SetMailFrom(_ context.Context, domain, mailFrom, behavior string) error {
	return s.edit(domain, func(id *denmaFakeIdentity) {
		id.MailFrom, id.MailFromStatus, id.MailFromMX, id.Since = mailFrom, "PENDING", behavior, time.Now()
	})
}

func (s *denmaFakeSES) RestartDKIM(_ context.Context, domain string) error {
	return s.edit(domain, func(id *denmaFakeIdentity) {
		id.DKIM, id.Verification, id.VerifyError, id.Tokens, id.Since = "PENDING", "PENDING", "", denmaFakeTokens(), time.Now()
	})
}

func (s *denmaFakeSES) SetConfigSet(_ context.Context, domain, set string) error {
	return s.edit(domain, func(id *denmaFakeIdentity) { id.ConfigSet = set })
}

func (s *denmaFakeSES) SetNotifications(_ context.Context, domain, topic string) error {
	return s.edit(domain, func(id *denmaFakeIdentity) { id.BounceTopic, id.ComplaintTopic, id.Headers = topic, topic, true })
}

func (s *denmaFakeSES) Delete(_ context.Context, domain string) error {
	_, err := s.db.Exec(`DELETE FROM denma.dev_ses_identities WHERE domain = $1`, domain)
	return err
}

// denmaDomainRef is what new domains copy: the configuration set and the
// bounce and complaint topic of the hub's sender's domain (Settings ->
// General), which reach the hub (cmd/denma_bounces.go).
type denmaDomainRef struct {
	Domain    string `json:"domain"`
	ConfigSet string `json:"config_set"`
	Topic     string `json:"topic"`
	Error     string `json:"error"`
}

var denmaRefCache struct {
	sync.Mutex
	ref denmaDomainRef
	at  time.Time
}

func (d *denmaCenters) domainRef(ctx context.Context, s denmaSES) denmaDomainRef {
	ref := denmaDomainRef{Domain: denmaSenderDomain(d.current().ko.String("app.from_email"))}
	c := &denmaRefCache
	c.Lock()
	defer c.Unlock()
	if c.ref.Domain == ref.Domain && c.ref.Error == "" && time.Since(c.at) < 10*time.Minute {
		return c.ref
	}
	if ref.Domain == "" {
		ref.Error = "The hub has no sender (Settings -> General -> Superadmin e-mails from)."
		return ref
	}
	id, err := s.Get(ctx, ref.Domain)
	switch {
	case err != nil:
		ref.Error = err.Error()
	case !id.Exists:
		ref.Error = ref.Domain + " isn't in Amazon SES."
	default:
		ref.ConfigSet, ref.Topic = id.ConfigSet, id.BounceTopic
		if ref.Topic == "" {
			ref.Error = ref.Domain + " sends its bounces nowhere."
		}
	}
	c.ref, c.at = ref, time.Now()
	return ref
}

// denmaDNSRecord is one of a domain's DNS records, and what DNS has there.
type denmaDNSRecord struct {
	Kind   string   `json:"kind"`   // dkim, mx, spf, dmarc
	Type   string   `json:"type"`   // CNAME, MX, TXT
	Name   string   `json:"name"`   // in full
	Host   string   `json:"host"`   // without the domain, as most DNS providers want it
	Value  string   `json:"value"`  // what to publish
	Status string   `json:"status"` // ok, missing, wrong, conflict
	Found  []string `json:"found"`  // what DNS has there
	Note   string   `json:"note"`
	SES    bool     `json:"ses"` // Amazon SES found it (so it's ok)
}

// denmaDomainIssue is something to fix in SES, and its fix (an action of
// DenmaFixDomain), if the hub has one.
type denmaDomainIssue struct {
	Text string `json:"text"`
	Fix  string `json:"fix"`
}

// denmaDomainState is a domain's last check.
type denmaDomainState struct {
	SES     denmaSESIdentity   `json:"ses"`
	Region  string             `json:"region"`
	Records []denmaDNSRecord   `json:"records"`
	Issues  []denmaDomainIssue `json:"issues"`
	Error   string             `json:"error"` // SES couldn't be asked

	// From the rest: whether it can send (SES has verified it), whether
	// nothing's left to do, and which (denmaDomainStatuses).
	CanSend bool   `json:"can_send"`
	Ready   bool   `json:"ready"`
	Status  string `json:"status"`
}

// denmaDomainStatuses are the statuses' labels and badges.
var denmaDomainStatuses = map[string][2]string{
	"":           {"Not checked yet", "status-draft"},
	"error":      {"Couldn't check", "status-draft"},
	"missing":    {"Not in Amazon SES", "status-cancelled"},
	"failed":     {"Failed", "status-cancelled"},
	"waiting":    {"Waiting for DNS", "status-scheduled"},
	"incomplete": {"Can send; to finish", "status-paused"},
	"ready":      {"Ready", "status-finished"},
}

// denmaLookups are DNS lookups with a time limit; a nil error with nothing
// found is a definite "none".
type denmaLookups struct {
	r *net.Resolver
}

func denmaDNSNone(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

func (l denmaLookups) txt(ctx context.Context, name string) ([]string, error) {
	out, err := l.r.LookupTXT(ctx, name)
	if denmaDNSNone(err) {
		return nil, nil
	}
	return out, err
}

func (l denmaLookups) cname(ctx context.Context, name string) (string, error) {
	out, err := l.r.LookupCNAME(ctx, name)
	if denmaDNSNone(err) {
		return "", nil
	}
	out = strings.ToLower(strings.TrimSuffix(out, "."))
	if out == strings.ToLower(name) {
		return "", err // no CNAME
	}
	return out, err
}

func (l denmaLookups) mx(ctx context.Context, name string) ([]string, error) {
	mxs, err := l.r.LookupMX(ctx, name)
	if denmaDNSNone(err) {
		return nil, nil
	}
	out := make([]string, len(mxs))
	for i, m := range mxs {
		out[i] = fmt.Sprintf("%d %s", m.Pref, strings.ToLower(strings.TrimSuffix(m.Host, ".")))
	}
	return out, err
}

// denmaHost is name without the domain: "abc._domainkey" for
// abc._domainkey.example.org.
func denmaHost(name, domain string) string {
	return strings.TrimSuffix(strings.TrimSuffix(name, domain), ".")
}

// denmaPrefixed is the lines of a TXT lookup that start with prefix (in any
// case): its SPF or DMARC records.
func denmaPrefixed(lines []string, prefix string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), strings.ToLower(prefix)) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// denmaMergeSPF adds SES to an SPF record, before its all: once evaluation
// reaches all, it stops.
func denmaMergeSPF(spf string) string {
	parts := strings.Fields(spf)
	for i, p := range parts {
		if strings.TrimLeft(strings.ToLower(p), "+-~?") == "all" {
			return strings.Join(append(append(append([]string{}, parts[:i]...), "include:amazonses.com"), parts[i:]...), " ")
		}
	}
	return spf + " include:amazonses.com"
}

// checkRecords looks the domain's records up in DNS (SES's own verdicts, when
// it has them, count first).
func checkDenmaRecords(ctx context.Context, domain, mailFrom, region string, id denmaSESIdentity) []denmaDNSRecord {
	l := denmaLookups{r: &net.Resolver{PreferGo: true}}
	var out []denmaDNSRecord
	lookupErr := func(r *denmaDNSRecord, err error) {
		r.Status, r.Note = "missing", "The DNS lookup didn't answer ("+err.Error()+"); it's tried again later."
	}

	// DKIM: three CNAMEs, from SES's tokens (none for a domain whose DKIM
	// keys SES doesn't manage).
	easy := id.DKIMOrigin == "" || id.DKIMOrigin == "AWS_SES"
	zone := id.SigningZone
	if zone == "" {
		zone = "dkim.amazonses.com"
	}
	for _, t := range id.Tokens {
		if !easy {
			break
		}
		r := denmaDNSRecord{Kind: "dkim", Type: "CNAME", Name: t + "._domainkey." + domain, Value: t + "." + zone}
		r.Host = denmaHost(r.Name, domain)
		got, err := l.cname(ctx, r.Name)
		switch {
		case id.DKIM == "SUCCESS":
			r.Status, r.SES = "ok", true
		case err != nil:
			lookupErr(&r, err)
		case got == r.Value:
			r.Status = "ok"
		case got != "":
			r.Status, r.Found = "wrong", []string{got}
		default:
			r.Status = "missing"
			// Some DNS providers add the domain to the name themselves.
			if twice, _ := l.cname(ctx, r.Name+"."+domain); twice != "" {
				r.Note = fmt.Sprintf("It's at %s.%s: the DNS provider added the domain again. Enter the name as %s.", r.Name, domain, r.Host)
			}
		}
		out = append(out, r)
	}

	// The bounce subdomain: SES's MX, alone, and SPF allowing SES.
	if mailFrom != "" {
		want := "10 feedback-smtp." + region + ".amazonses.com"
		r := denmaDNSRecord{Kind: "mx", Type: "MX", Name: mailFrom, Host: denmaHost(mailFrom, domain), Value: want}
		got, err := l.mx(ctx, mailFrom)
		r.Found = got
		has := false
		for _, g := range got {
			has = has || strings.HasSuffix(g, " feedback-smtp."+region+".amazonses.com")
		}
		switch {
		case id.MailFromStatus == "SUCCESS" && len(got) <= 1:
			r.Status, r.SES = "ok", true
		case err != nil:
			lookupErr(&r, err)
		case len(got) == 0:
			r.Status = "missing"
		case has && len(got) == 1:
			r.Status = "ok"
		default:
			r.Status, r.Note = "conflict", "Amazon SES's MX record has to be the only one here. If this name receives mail, choose another bounce subdomain instead of removing the others."
		}
		out = append(out, r)

		r = denmaDNSRecord{Kind: "spf", Type: "TXT", Name: mailFrom, Host: denmaHost(mailFrom, domain), Value: denmaSPFValue}
		txt, err := l.txt(ctx, mailFrom)
		spf := denmaPrefixed(txt, "v=spf1")
		r.Found = spf
		switch {
		case err != nil:
			lookupErr(&r, err)
		case len(spf) == 0:
			r.Status = "missing"
		case len(spf) > 1:
			r.Status, r.Value = "conflict", denmaMergeSPF(spf[0])
			r.Note = "There's more than one SPF record here, which fails SPF. Replace them with one, such as this."
		case strings.Contains(strings.ToLower(spf[0]), "include:amazonses.com"):
			r.Status, r.Value = "ok", spf[0]
		default:
			r.Status, r.Value = "wrong", denmaMergeSPF(spf[0])
			r.Note = "There's an SPF record here without Amazon SES. Change it to this one (one SPF record only), rather than adding a second."
		}
		out = append(out, r)
	}

	// DMARC: one record, p=none or stronger (an existing one is kept).
	r := denmaDNSRecord{Kind: "dmarc", Type: "TXT", Name: "_dmarc." + domain, Host: "_dmarc", Value: denmaDMARCValue}
	txt, err := l.txt(ctx, r.Name)
	dmarc := denmaPrefixed(txt, "v=DMARC1")
	r.Found = dmarc
	switch {
	case err != nil:
		lookupErr(&r, err)
	case len(dmarc) == 0:
		r.Status = "missing"
	case len(dmarc) > 1:
		r.Status, r.Value = "conflict", ""
		r.Note = "There's more than one DMARC record, so mail providers apply none. Keep only one: the domain's owner chooses which."
	default:
		policies := 0
		valid := false
		strictSPF := false
		for _, t := range strings.Split(dmarc[0], ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(t), "=")
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "p":
				policies++
				v = strings.ToLower(strings.TrimSpace(v))
				valid = v == "none" || v == "quarantine" || v == "reject"
			case "aspf":
				strictSPF = strings.EqualFold(strings.TrimSpace(v), "s")
			}
		}
		if policies == 1 && valid {
			r.Status, r.Value = "ok", dmarc[0]
			if strictSPF {
				r.Note = "It has aspf=s, so SPF can't align with the bounce subdomain; DMARC passes on DKIM alone."
			}
		} else {
			r.Status = "wrong"
			r.Note = "This DMARC record has no valid policy (p=none, quarantine or reject). Replace it with this one, or fix its policy."
		}
	}
	return append(out, r)
}

// checkDomain asks SES and DNS about a domain and saves what they say.
func (d *denmaCenters) checkDomain(ctx context.Context, s denmaSES, domain string) (denmaDomainState, error) {
	var mailFrom string
	if err := d.db().Get(&mailFrom, `SELECT mail_from FROM denma.sending_domains WHERE domain = $1`, domain); err != nil {
		return denmaDomainState{}, err
	}
	st := denmaDomainState{Region: s.Region()}
	id, err := s.Get(ctx, domain)
	if err != nil {
		// Keep what was known.
		var old []byte
		_ = d.db().Get(&old, `SELECT state FROM denma.sending_domains WHERE domain = $1`, domain)
		_ = json.Unmarshal(old, &st)
		st.Error = err.Error()
	} else {
		st.SES, st.Error = id, ""
		if id.MailFrom != "" && id.MailFrom != mailFrom {
			mailFrom = id.MailFrom // SES's own, which the hub doesn't change unasked
		}
		st.Records = checkDenmaRecords(ctx, domain, mailFrom, s.Region(), id)
		st.Issues = denmaIssues(id, d.domainRef(ctx, s))
	}
	denmaSendingVerdict(&st)

	b, _ := json.Marshal(st)
	if _, err := d.db().Exec(`UPDATE denma.sending_domains SET state = $2, mail_from = $3, checked_at = NOW() WHERE domain = $1`,
		domain, b, mailFrom); err != nil {
		return st, err
	}
	return st, nil
}

// denmaIssues are what SES has wrong, and the hub's fix for each.
func denmaIssues(id denmaSESIdentity, ref denmaDomainRef) []denmaDomainIssue {
	if !id.Exists {
		return []denmaDomainIssue{{Text: "Amazon SES doesn't have this domain.", Fix: "setup"}}
	}
	var out []denmaDomainIssue
	easy := id.DKIMOrigin == "" || id.DKIMOrigin == "AWS_SES"
	switch {
	case !easy && id.DKIM != "SUCCESS":
		out = append(out, denmaDomainIssue{Text: fmt.Sprintf("Its DKIM keys aren't managed by Amazon SES (%s), and DKIM is %s. Fix it in the AWS console.", id.DKIMOrigin, strings.ToLower(id.DKIM))})
	case id.DKIM == "FAILED":
		out = append(out, denmaDomainIssue{Text: "Amazon SES stopped looking for the DKIM records (it looks for 72 hours). Once they're all published (below), start it again; its records may change.", Fix: "dkim"})
	case id.DKIM == "NOT_STARTED" || id.DKIM == "":
		out = append(out, denmaDomainIssue{Text: "DKIM was never started for this domain.", Fix: "dkim"})
	}
	switch {
	case id.MailFrom == "":
		out = append(out, denmaDomainIssue{Text: "It has no bounce subdomain, so its bounces' return address is Amazon SES's (and SPF can't align for DMARC).", Fix: "mail_from"})
	case id.MailFromStatus == "FAILED":
		out = append(out, denmaDomainIssue{Text: "Amazon SES stopped looking for the bounce subdomain's MX record. Once it's published (below), start it again.", Fix: "mail_from"})
	}
	if ref.Error == "" {
		if id.BounceTopic != ref.Topic || id.ComplaintTopic != ref.Topic || !id.Headers {
			out = append(out, denmaDomainIssue{Text: "Its bounces and complaints don't reach the hub (with their headers), as " + ref.Domain + "'s do.", Fix: "notifications"})
		}
		if ref.ConfigSet != "" && id.ConfigSet != ref.ConfigSet {
			out = append(out, denmaDomainIssue{Text: fmt.Sprintf("It doesn't use the configuration set %s, as %s does.", ref.ConfigSet, ref.Domain), Fix: "config_set"})
		}
	}
	return out
}

// denmaSendingVerdict sets whether a domain can send and its status.
func denmaSendingVerdict(st *denmaDomainState) {
	id := st.SES
	st.CanSend = id.Exists && id.Verified && id.DKIM == "SUCCESS"
	recordsOK := true
	for _, r := range st.Records {
		recordsOK = recordsOK && r.Status == "ok"
	}
	st.Ready = st.CanSend && id.MailFromStatus == "SUCCESS" && recordsOK && len(st.Issues) == 0
	switch {
	case st.Error != "" && !id.Exists:
		st.Status = "error"
	case !id.Exists:
		st.Status = "missing"
	case st.Ready:
		st.Status = "ready"
	case st.CanSend:
		st.Status = "incomplete"
	case id.DKIM == "FAILED" || id.Verification == "FAILED" || id.MailFromStatus == "FAILED":
		st.Status = "failed"
	default:
		st.Status = "waiting"
	}
}

// setupDomain sets a domain up in SES, adding only what it hasn't: the
// domain (Easy DKIM), its bounce subdomain, the configuration set and
// notifications of the hub's sender's domain. It doesn't change what SES
// has already (another bounce subdomain, topic or set), which may be in use.
func (d *denmaCenters) setupDomain(ctx context.Context, s denmaSES, domain, mailFrom string) error {
	ref := d.domainRef(ctx, s)
	id, err := s.Get(ctx, domain)
	if err != nil {
		return err
	}
	if !id.Exists {
		if err := s.Create(ctx, domain, ref.ConfigSet); err != nil {
			return err
		}
		if id, err = s.Get(ctx, domain); err != nil {
			return err
		}
	}
	if (id.DKIMOrigin == "" || id.DKIMOrigin == "AWS_SES") && id.DKIM == "NOT_STARTED" {
		if err := s.RestartDKIM(ctx, domain); err != nil {
			return err
		}
	}
	if id.MailFrom == "" {
		if err := s.SetMailFrom(ctx, domain, mailFrom, string(sesv2types.BehaviorOnMxFailureUseDefaultValue)); err != nil {
			return err
		}
	}
	if id.ConfigSet == "" && ref.ConfigSet != "" {
		if err := s.SetConfigSet(ctx, domain, ref.ConfigSet); err != nil {
			return err
		}
	}
	if ref.Topic != "" && id.BounceTopic == "" && id.ComplaintTopic == "" {
		if err := s.SetNotifications(ctx, domain, ref.Topic); err != nil {
			return err
		}
	}
	return nil
}

// denmaSenderDomain is the domain of a From address, lowercase; "" if it
// isn't one.
func denmaSenderDomain(from string) string {
	addr, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return ""
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(addr.Address[at+1:])
}

// denmaCheckMailFrom checks a bounce subdomain: one or more labels below the
// domain.
func denmaCheckMailFrom(mailFrom, domain string) error {
	if !reDenmaDomain.MatchString(mailFrom) || !strings.HasSuffix(mailFrom, "."+domain) || len(mailFrom) > 253 {
		return fmt.Errorf("The bounce subdomain has to be under %s, such as bounce.%s.", domain, domain)
	}
	return nil
}

// denmaCenterSender is a center's sender.
type denmaCenterSender struct {
	ID     int    `db:"id"`
	Slug   string `db:"slug"`
	Name   string `db:"name"`
	From   string `db:"from_email"`
	Domain string `db:"-"`
}

// centerSenders reads every center's sender from its settings, in one query.
func (d *denmaCenters) centerSenders() ([]denmaCenterSender, error) {
	var ctrs []struct {
		ID     int    `db:"id"`
		Slug   string `db:"slug"`
		Name   string `db:"name"`
		Schema string `db:"schema_name"`
	}
	if err := d.db().Select(&ctrs, `SELECT id, slug, name, schema_name FROM denma.centers
		WHERE TO_REGCLASS(QUOTE_IDENT(schema_name) || '.settings') IS NOT NULL`); err != nil {
		return nil, err
	}
	if len(ctrs) == 0 {
		return nil, nil
	}
	parts := make([]string, len(ctrs))
	for i, c := range ctrs {
		parts[i] = fmt.Sprintf(`SELECT %d AS id, %s AS slug, %s AS name, COALESCE((SELECT value #>> '{}' FROM %s.settings WHERE key = 'app.from_email'), '') AS from_email`,
			c.ID, pq.QuoteLiteral(c.Slug), pq.QuoteLiteral(c.Name), pq.QuoteIdentifier(c.Schema))
	}
	var out []denmaCenterSender
	if err := d.db().Select(&out, strings.Join(parts, " UNION ALL ")); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Domain = denmaSenderDomain(out[i].From)
	}
	return out, nil
}

// adoptSenders adds the domains of senders set before (every center's and
// the hub's), for their centers.
func (d *denmaCenters) adoptSenders() error {
	senders, err := d.centerSenders()
	if err != nil {
		return err
	}
	add := func(domain string, centerID int) error {
		if domain == "" || !reDenmaDomain.MatchString(domain) {
			return nil
		}
		res, err := d.db().Exec(`INSERT INTO denma.sending_domains (domain, mail_from) VALUES ($1, $2) ON CONFLICT DO NOTHING`, domain, "bounce."+domain)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			lo.Printf("denma: added %s to the sending domains (a sender's)", domain)
		}
		if centerID > 0 {
			_, err = d.db().Exec(`INSERT INTO denma.sending_domain_centers (domain, center_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, domain, centerID)
		}
		return err
	}
	for _, s := range senders {
		if err := add(s.Domain, s.ID); err != nil {
			return err
		}
	}
	return add(denmaSenderDomain(d.current().ko.String("app.from_email")), 0)
}

// watchDomains adds the senders' domains, then checks those due: every 2
// minutes in a new domain's first hour, every 15 until it's ready, then daily.
func (d *denmaCenters) watchDomains() {
	time.Sleep(15 * time.Second)
	adopted := false
	for {
		if s, _ := d.ses(); s != nil {
			if !adopted {
				if err := d.adoptSenders(); err != nil {
					lo.Printf("denma: error adding the senders' domains: %v", err)
				} else {
					adopted = true
				}
			}
			d.checkDue(s, 10)
		}
		time.Sleep(time.Minute)
	}
}

func (d *denmaCenters) checkDue(s denmaSES, n int) {
	var due []string
	if err := d.db().Select(&due, `SELECT domain FROM denma.sending_domains WHERE checked_at IS NULL OR checked_at < NOW() -
		CASE WHEN (state->>'ready')::BOOLEAN THEN INTERVAL '1 day'
			WHEN created_at > NOW() - INTERVAL '1 hour' THEN INTERVAL '2 minutes'
			ELSE INTERVAL '15 minutes' END
		ORDER BY checked_at NULLS FIRST LIMIT $1`, n); err != nil {
		lo.Printf("denma: error listing the sending domains to check: %v", err)
		return
	}
	for i, dom := range due {
		if i > 0 {
			time.Sleep(time.Second) // SES's notification reads: one a second
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		before := d.domainCanSend(dom)
		st, err := d.checkDomain(ctx, s, dom)
		cancel()
		if err != nil {
			lo.Printf("denma: error checking the sending domain %s: %v", dom, err)
			continue
		}
		if st.CanSend && !before {
			lo.Printf("denma: Amazon SES has verified the sending domain %s", dom)
		}
	}
}

// domainCanSend is a domain's last verdict.
func (d *denmaCenters) domainCanSend(domain string) bool {
	var ok bool
	_ = d.db().Get(&ok, `SELECT COALESCE((state->>'can_send')::BOOLEAN, false) FROM denma.sending_domains WHERE domain = $1`, domain)
	return ok
}

// addDomain adds a domain for centers (IDs), sets it up in SES and checks it.
// It's added even if SES can't be reached, and the error returned.
func (d *denmaCenters) addDomain(domain, mailFrom string, centers []int) (denmaDomainState, error) {
	if _, err := d.db().Exec(`INSERT INTO denma.sending_domains (domain, mail_from) VALUES ($1, $2) ON CONFLICT DO NOTHING`, domain, mailFrom); err != nil {
		return denmaDomainState{}, err
	}
	for _, id := range centers {
		if _, err := d.db().Exec(`INSERT INTO denma.sending_domain_centers (domain, center_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, domain, id); err != nil {
			return denmaDomainState{}, err
		}
	}
	s, why := d.ses()
	if s == nil {
		return denmaDomainState{}, errors.New(why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	setupErr := d.setupDomain(ctx, s, domain, mailFrom)
	st, err := d.checkDomain(ctx, s, domain)
	if setupErr != nil {
		return st, setupErr
	}
	return st, err
}

// The pages and the API.

func initDenmaDomainHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/domains"), a.ViewDenmaDomains)
	g.GET(path.Join(uriAdmin, "/domains/:domain"), a.ViewDenmaDomain)
}

func initDenmaDomainAPIHandlers(g *echo.Group, a *App) {
	g.POST("/api/denma/domains", a.DenmaAddDomain)
	g.POST("/api/denma/domains/:domain/check", a.DenmaCheckDomain)
	g.POST("/api/denma/domains/:domain/fix", a.DenmaFixDomain)
	g.POST("/api/denma/domains/:domain/centers", a.DenmaAddDomainCenter)
	g.DELETE("/api/denma/domains/:domain/centers/:slug", a.DenmaRemoveDomainCenter)
	g.DELETE("/api/denma/domains/:domain", a.DenmaDeleteDomain)
}

// denmaDomainRow is a domain on the pages.
type denmaDomainRow struct {
	Domain    string             `json:"domain"`
	MailFrom  string             `json:"mail_from"`
	State     denmaDomainState   `json:"state"`
	Label     string             `json:"label"`
	Badge     string             `json:"badge"`
	CheckedAt *time.Time         `json:"checked_at"`
	CreatedAt time.Time          `json:"created_at"`
	Centers   []denmaOtherCenter `json:"centers"` // it's for
	Senders   []denmaOtherCenter `json:"senders"` // whose sender is on it
	Hub       bool               `json:"hub"`     // the hub's sender is on it
}

// domainRows reads the domains (or one).
func (d *denmaCenters) domainRows(only string) ([]denmaDomainRow, error) {
	var rows []struct {
		Domain    string     `db:"domain"`
		MailFrom  string     `db:"mail_from"`
		State     []byte     `db:"state"`
		CheckedAt *time.Time `db:"checked_at"`
		CreatedAt time.Time  `db:"created_at"`
	}
	if err := d.db().Select(&rows, `SELECT domain, mail_from, state, checked_at, created_at FROM denma.sending_domains
		WHERE $1 = '' OR domain = $1 ORDER BY domain`, only); err != nil {
		return nil, err
	}
	var links []struct {
		Domain string `db:"domain"`
		denmaOtherCenter
	}
	if err := d.db().Select(&links, `SELECT dc.domain, c.slug, c.name FROM denma.sending_domain_centers dc
		JOIN denma.centers c ON c.id = dc.center_id WHERE $1 = '' OR dc.domain = $1 ORDER BY LOWER(c.name)`, only); err != nil {
		return nil, err
	}
	senders, err := d.centerSenders()
	if err != nil {
		return nil, err
	}
	hub := denmaSenderDomain(d.current().ko.String("app.from_email"))

	out := make([]denmaDomainRow, len(rows))
	for i, r := range rows {
		x := denmaDomainRow{Domain: r.Domain, MailFrom: r.MailFrom, CheckedAt: r.CheckedAt, CreatedAt: r.CreatedAt,
			Centers: []denmaOtherCenter{}, Senders: []denmaOtherCenter{}, Hub: r.Domain == hub}
		_ = json.Unmarshal(r.State, &x.State)
		if r.CheckedAt == nil {
			x.State.Status = ""
		}
		s := denmaDomainStatuses[x.State.Status]
		x.Label, x.Badge = s[0], s[1]
		for _, l := range links {
			if l.Domain == r.Domain {
				x.Centers = append(x.Centers, l.denmaOtherCenter)
			}
		}
		for _, s := range senders {
			if s.Domain == r.Domain {
				x.Senders = append(x.Senders, denmaOtherCenter{Slug: s.Slug, Name: s.Name})
			}
		}
		sort.Slice(x.Senders, func(i, j int) bool { return strings.ToLower(x.Senders[i].Name) < strings.ToLower(x.Senders[j].Name) })
		out[i] = x
	}
	return out, nil
}

type denmaDomainsView struct {
	adminView
	Domains []denmaDomainRow
	Centers []denmaOtherCenter
	Region  string
	Off     string // why there's no SES
	Ref     denmaDomainRef
}

// ViewDenmaDomains renders the Sending domains page (views/denma-domains.html).
func (a *App) ViewDenmaDomains(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	v, err := d.domainsView(c, "", "Sending domains")
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "admin-denma-domains", v)
}

// ViewDenmaDomain renders a domain's page (views/denma-domain.html): its DNS
// records, SES's checks and its centers.
func (a *App) ViewDenmaDomain(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom := strings.ToLower(c.Param("domain"))
	v, err := d.domainsView(c, dom, dom)
	if err != nil {
		return err
	}
	if len(v.Domains) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "That isn't one of the sending domains.")
	}
	return c.Render(http.StatusOK, "admin-denma-domain", v)
}

func (d *denmaCenters) domainsView(c echo.Context, only, title string) (denmaDomainsView, error) {
	rows, err := d.domainRows(only)
	if err != nil {
		return denmaDomainsView{}, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	v := denmaDomainsView{adminView: newAdminView(c, title, "", "denma.domains"), Domains: rows, Centers: []denmaOtherCenter{}}
	if err := d.db().Select(&v.Centers, `SELECT slug, name FROM denma.centers ORDER BY LOWER(name)`); err != nil {
		return v, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	s, why := d.ses()
	if s == nil {
		v.Off = why
		return v, nil
	}
	v.Region = s.Region()
	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()
	v.Ref = d.domainRef(ctx, s)
	return v, nil
}

// denmaDomainParam is the :domain of a request, if it's one of the sending
// domains.
func (d *denmaCenters) domainParam(c echo.Context) (string, error) {
	dom := strings.ToLower(c.Param("domain"))
	var ok bool
	if err := d.db().Get(&ok, `SELECT EXISTS (SELECT 1 FROM denma.sending_domains WHERE domain = $1)`, dom); err != nil {
		return "", echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if !ok {
		return "", echo.NewHTTPError(http.StatusNotFound, "That isn't one of the sending domains.")
	}
	return dom, nil
}

func (d *denmaCenters) domainResp(c echo.Context, dom string, warn error) error {
	rows, err := d.domainRows(dom)
	if err != nil || len(rows) == 0 {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("reading %s: %v", dom, err))
	}
	out := map[string]any{"domain": rows[0]}
	if warn != nil {
		out["warning"] = warn.Error()
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaAddDomain adds a sending domain, for the centers chosen, and sets it
// up in SES.
func (a *App) DenmaAddDomain(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	var req struct {
		Domain   string   `json:"domain"`
		MailFrom string   `json:"mail_from"`
		Centers  []string `json:"centers"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	dom := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Domain)), ".")
	if !reDenmaDomain.MatchString(dom) || len(dom) > 253 {
		return echo.NewHTTPError(http.StatusBadRequest, "Enter a domain, such as example.org: letters, digits, hyphens and dots.")
	}
	mailFrom := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.MailFrom)), ".")
	if mailFrom == "" {
		mailFrom = "bounce." + dom
	}
	if err := denmaCheckMailFrom(mailFrom, dom); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	var exists bool
	if err := d.db().Get(&exists, `SELECT EXISTS (SELECT 1 FROM denma.sending_domains WHERE domain = $1)`, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if exists {
		return echo.NewHTTPError(http.StatusConflict, dom+" is already one of the sending domains.")
	}
	var ids []int
	if len(req.Centers) > 0 {
		if err := d.db().Select(&ids, `SELECT id FROM denma.centers WHERE slug = ANY($1)`, pq.Array(req.Centers)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if s, why := d.ses(); s == nil {
		return echo.NewHTTPError(http.StatusBadRequest, why)
	}
	_, err = d.addDomain(dom, mailFrom, ids)
	if err != nil {
		err = fmt.Errorf("It was added, but Amazon SES didn't take all of it: %v", err)
	}
	return d.domainResp(c, dom, err)
}

// DenmaCheckDomain asks SES and DNS about a domain now.
func (a *App) DenmaCheckDomain(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom, err := d.domainParam(c)
	if err != nil {
		return err
	}
	s, why := d.ses()
	if s == nil {
		return echo.NewHTTPError(http.StatusBadRequest, why)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()
	if _, err := d.checkDomain(ctx, s, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return d.domainResp(c, dom, nil)
}

// DenmaFixDomain fixes one of a domain's issues in SES (denmaIssues): setup
// (add it), dkim (start DKIM again), mail_from (start the bounce subdomain
// again, or change it to mail_from), notifications or config_set (as the hub's
// sender's domain). Each only when SES is in the state it's for.
func (a *App) DenmaFixDomain(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom, err := d.domainParam(c)
	if err != nil {
		return err
	}
	var req struct {
		Action   string `json:"action"`
		MailFrom string `json:"mail_from"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	s, why := d.ses()
	if s == nil {
		return echo.NewHTTPError(http.StatusBadRequest, why)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 45*time.Second)
	defer cancel()
	id, err := s.Get(ctx, dom)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "Amazon SES: "+err.Error())
	}
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	ref := d.domainRef(ctx, s)
	var mailFrom string
	if err := d.db().Get(&mailFrom, `SELECT mail_from FROM denma.sending_domains WHERE domain = $1`, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	switch req.Action {
	case "setup":
		err = d.setupDomain(ctx, s, dom, mailFrom)
	case "dkim":
		// Never for a domain SES has verified, whose records would change.
		if !id.Exists || (id.DKIMOrigin != "" && id.DKIMOrigin != "AWS_SES") || (id.DKIM != "FAILED" && id.DKIM != "NOT_STARTED") {
			return bad("DKIM can only be started again once it has failed.")
		}
		err = s.RestartDKIM(ctx, dom)
	case "mail_from":
		next := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.MailFrom)), ".")
		if next == "" {
			next = id.MailFrom
		}
		if next == "" {
			next = mailFrom
		}
		if err := denmaCheckMailFrom(next, dom); err != nil {
			return bad(err.Error())
		}
		if !id.Exists {
			return bad("Amazon SES doesn't have this domain yet.")
		}
		if next == id.MailFrom && id.MailFromStatus != "FAILED" {
			return bad("That's its bounce subdomain already; Amazon SES is still looking for its MX record.")
		}
		behavior := id.MailFromMX // kept, as is
		if behavior == "" {
			behavior = string(sesv2types.BehaviorOnMxFailureUseDefaultValue)
		}
		if err = s.SetMailFrom(ctx, dom, next, behavior); err == nil {
			_, err = d.db().Exec(`UPDATE denma.sending_domains SET mail_from = $2 WHERE domain = $1`, dom, next)
		}
	case "notifications":
		if ref.Error != "" || !id.Exists {
			return bad("There's nothing to copy: " + ref.Error)
		}
		err = s.SetNotifications(ctx, dom, ref.Topic)
	case "config_set":
		if ref.Error != "" || ref.ConfigSet == "" || !id.Exists {
			return bad("There's no configuration set to use.")
		}
		err = s.SetConfigSet(ctx, dom, ref.ConfigSet)
	default:
		return bad("unknown action")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "Amazon SES: "+err.Error())
	}
	if _, err := d.checkDomain(ctx, s, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return d.domainResp(c, dom, nil)
}

// DenmaAddDomainCenter lets a center send from a domain.
func (a *App) DenmaAddDomainCenter(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom, err := d.domainParam(c)
	if err != nil {
		return err
	}
	var req struct {
		Slug string `json:"slug"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	res, err := d.db().Exec(`INSERT INTO denma.sending_domain_centers (domain, center_id)
		SELECT $1, id FROM denma.centers WHERE slug = $2 ON CONFLICT DO NOTHING`, dom, req.Slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "That center is already on it, or isn't a center.")
	}
	return d.domainResp(c, dom, nil)
}

// DenmaRemoveDomainCenter stops a center sending from a domain, unless its
// sender is on it.
func (a *App) DenmaRemoveDomainCenter(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom, err := d.domainParam(c)
	if err != nil {
		return err
	}
	slug := c.Param("slug")
	senders, err := d.centerSenders()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for _, s := range senders {
		if s.Slug == slug && s.Domain == dom {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s's sender, %s, is on %s. Change it on its Config page first.", s.Name, s.From, dom))
		}
	}
	if _, err := d.db().Exec(`DELETE FROM denma.sending_domain_centers WHERE domain = $1
		AND center_id = (SELECT id FROM denma.centers WHERE slug = $2)`, dom, slug); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return d.domainResp(c, dom, nil)
}

// DenmaDeleteDomain removes a sending domain from the hub, and with ses=true
// from SES too, unless a sender (the hub's or a center's) is on it.
func (a *App) DenmaDeleteDomain(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	dom, err := d.domainParam(c)
	if err != nil {
		return err
	}
	if denmaSenderDomain(d.current().ko.String("app.from_email")) == dom {
		return echo.NewHTTPError(http.StatusBadRequest, "The hub's own sender (Settings -> General) is on "+dom+".")
	}
	senders, err := d.centerSenders()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var using []string
	for _, s := range senders {
		if s.Domain == dom {
			using = append(using, s.Name)
		}
	}
	if len(using) > 0 {
		if len(using) == 1 {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s's sender is on %s. Change it on its Config page first.", using[0], dom))
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("The senders of %s are on %s. Change them on their Config pages first.", strings.Join(using, ", "), dom))
	}
	if c.QueryParam("ses") == "true" {
		s, why := d.ses()
		if s == nil {
			return echo.NewHTTPError(http.StatusBadRequest, why)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
		defer cancel()
		if err := s.Delete(ctx, dom); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "Amazon SES: "+err.Error())
		}
	}
	if _, err := d.db().Exec(`DELETE FROM denma.sending_domains WHERE domain = $1`, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// In the centers: senders on their domains.

// denmaCenterDomain is one of a center's sending domains, for its Config
// page.
type denmaCenterDomain struct {
	Domain  string `db:"domain" json:"domain"`
	Status  string `db:"status" json:"status"`
	CanSend bool   `db:"can_send" json:"can_send"`
	Label   string `db:"-" json:"label"`
	Badge   string `db:"-" json:"badge"`
}

// denmaCenterDomains are the center's sending domains (none for the hub, or
// without SES).
func (a *App) denmaCenterDomains() ([]denmaCenterDomain, error) {
	ctr := a.denmaCenterOf()
	if ctr == nil || !denmaDomainsOn() {
		return nil, nil
	}
	var out []denmaCenterDomain
	if err := a.db.Select(&out, `SELECT d.domain,
			CASE WHEN d.checked_at IS NULL THEN '' ELSE COALESCE(d.state->>'status', '') END AS status,
			COALESCE((d.state->>'can_send')::BOOLEAN, false) AS can_send
		FROM denma.sending_domain_centers dc JOIN denma.sending_domains d ON d.domain = dc.domain
		WHERE dc.center_id = $1 ORDER BY d.domain`, ctr.ID); err != nil {
		return nil, err
	}
	for i := range out {
		st := denmaDomainStatuses[out[i].Status]
		out[i].Label, out[i].Badge = st[0], st[1]
	}
	return out, nil
}

// denmaCheckSender refuses a sender (From) whose domain isn't one of the
// center's sending domains. Its errors are for people (denmaBadRequest), or
// HTTP errors.
func (a *App) denmaCheckSender(from string) error {
	dom := denmaSenderDomain(from)
	if dom == "" || a.denmaCenterOf() == nil || !denmaDomainsOn() {
		return nil // listmonk's own checks say what's wrong with a bad address
	}
	doms, err := a.denmaCenterDomains()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	names := make([]string, 0, len(doms))
	for _, d := range doms {
		if d.Domain == dom {
			return nil
		}
		names = append(names, d.Domain)
	}
	have := "This center has no sending domains yet."
	if len(names) > 0 {
		have = "This center can send from " + strings.Join(names, ", ") + "."
	}
	return fmt.Errorf("The sender has to be at one of the center's domains, which Amazon SES has to verify, and %s isn't one. %s A superadmin adds domains for centers on the hub's Sending domains page.", dom, have)
}

// denmaCheckSenderReady refuses to send (start a campaign, send a test) from
// a domain SES hasn't verified, if it has said so. If SES couldn't be asked,
// it's sent: SES itself refuses what it hasn't verified.
func (a *App) denmaCheckSenderReady(from string) error {
	if err := a.denmaCheckSender(from); err != nil {
		return err
	}
	dom := denmaSenderDomain(from)
	if dom == "" || a.denmaCenterOf() == nil || !denmaDomainsOn() {
		return nil
	}
	if !a.denmaDomainRefused(dom) {
		return nil
	}
	return fmt.Errorf("Amazon SES hasn't verified %s yet, so nothing can be sent from it. Its DNS records are on the hub's Sending domains page; it's checked again every few minutes.", dom)
}

// denmaDomainRefused reports whether SES has said it hasn't verified a
// sending domain (false if it's not been checked yet, or SES couldn't be
// asked: SES itself refuses what it hasn't verified).
func (a *App) denmaDomainRefused(dom string) bool {
	var st struct {
		Checked bool   `db:"checked"`
		State   []byte `db:"state"`
	}
	if err := a.db.Get(&st, `SELECT checked_at IS NOT NULL AS checked, state FROM denma.sending_domains WHERE domain = $1`, dom); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error reading the sending domain %s: %v", dom, err)
		}
		return false
	}
	var s denmaDomainState
	_ = json.Unmarshal(st.State, &s)
	return st.Checked && !s.CanSend && !(s.Error != "" && !s.SES.Exists)
}

// denmaSystemSender is who a center's own e-mails are from: opt-in
// confirmations (re-subscribing too), notifications, invites, password resets
// and data exports. That's its sender, unless its domain isn't one of its
// sending domains or SES has said it hasn't verified it (a new center's, at
// first): then the hub's address (its "Superadmin e-mails from"), under the
// center's name, with the center's sender as the Reply-To. Campaigns,
// automations and transactional messages never use the hub's address: they
// wait for, or are refused until, the center's domain.
func (a *App) denmaSystemSender() (from, replyTo string) {
	from = a.cfg.FromEmail
	ctr := a.denmaCenterOf()
	if ctr == nil || denmaHub == nil || !denmaDomainsOn() {
		return from, ""
	}
	dom := denmaSenderDomain(from)
	var mine bool
	if err := a.db.Get(&mine, `SELECT EXISTS (SELECT 1 FROM denma.sending_domain_centers WHERE center_id = $1 AND domain = $2)`, ctr.ID, dom); err != nil {
		a.log.Printf("denma: error reading the center's sending domains: %v", err)
		return from, ""
	}
	if mine && !a.denmaDomainRefused(dom) {
		return from, ""
	}
	hub, err := mail.ParseAddress(denmaHub.current().cfg.FromEmail)
	if err != nil {
		return from, ""
	}
	name := a.cfg.SiteName
	if addr, err := mail.ParseAddress(from); err == nil && addr.Name != "" {
		name = addr.Name
	}
	return (&mail.Address{Name: name, Address: hub.Address}).String(), from
}

// denmaSetSystemSender sets a center's own e-mail's sender, and Reply-To if
// any (denmaSystemSender).
func (a *App) denmaSetSystemSender(m *models.Message) {
	from, replyTo := a.denmaSystemSender()
	m.From = from
	if replyTo != "" {
		if m.Headers == nil {
			m.Headers = textproto.MIMEHeader{}
		}
		m.Headers.Set("Reply-To", replyTo)
	}
}

// denmaCheckHeaders refuses a center's message headers (a campaign's, or a
// transactional message's) that would change who it's from, past the sender
// checks: From, Sender, Return-Path, Resent-From, Resent-Sender and X-SES-*
// (email.DenmaSenderHeader). The e-mail messenger leaves them out anyway.
func (a *App) denmaCheckHeaders(hdrs models.Headers) error {
	if a.denmaCenterOf() == nil {
		return nil
	}
	for _, set := range hdrs {
		for k := range set {
			if email.DenmaSenderHeader(k) {
				return fmt.Errorf("The header %s can't be set: it would change who the e-mail is from, or how Amazon SES sends it. The sender is set by From, and Reply-To can be set.", strings.TrimSpace(k))
			}
		}
	}
	return nil
}

// denmaCheckHubSender refuses a hub sender on a domain that isn't one of the
// sending domains.
func (a *App) denmaCheckHubSender(from string) error {
	if a.denmaInCenter() || !denmaDomainsOn() {
		return nil
	}
	dom := denmaSenderDomain(from)
	var ok bool
	if err := a.db.Get(&ok, `SELECT EXISTS (SELECT 1 FROM denma.sending_domains WHERE domain = $1)`, dom); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if !ok {
		return fmt.Errorf("%s isn't one of the sending domains. Add it on the Sending domains page first.", dom)
	}
	return nil
}

// denmaBadRequest is a sender check's error as an HTTP error: a 400 with its
// words, unless it's one already.
func denmaBadRequest(err error) error {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return he
	}
	return echo.NewHTTPError(http.StatusBadRequest, err.Error())
}

// denmaCheckCampaignStart refuses to start or schedule a campaign whose
// sender's domain SES hasn't verified.
func (a *App) denmaCheckCampaignStart(id int, status string) error {
	if status != models.CampaignStatusRunning && status != models.CampaignStatusScheduled {
		return nil
	}
	if a.denmaCenterOf() == nil || !denmaDomainsOn() {
		return nil
	}
	camp, err := a.core.GetCampaign(id, "", "")
	if err != nil {
		return err
	}
	if err := a.denmaCheckSenderReady(camp.FromEmail); err != nil {
		return denmaBadRequest(err)
	}
	return nil
}
