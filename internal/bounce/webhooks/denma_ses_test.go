package webhooks

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

const denmaTestCertURL = "https://sns.ca-central-1.amazonaws.com/SimpleNotificationService-denmatest.pem"

// denmaTestSES is an SES whose certificate cache holds a test certificate, and its key.
func denmaTestSES(t *testing.T) (*SES, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sns test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSES()
	s.certs["/SimpleNotificationService-denmatest.pem"] = cert
	return s, key
}

// denmaSign signs a notification as SNS does, with its SignatureVersion.
func denmaSign(t *testing.T, s *SES, key *rsa.PrivateKey, n sesNotif) []byte {
	t.Helper()
	n.SigningCertURL = denmaTestCertURL
	h, sum := crypto.SHA1, []byte(nil)
	if n.SignatureVersion == "2" {
		h = crypto.SHA256
		d := sha256.Sum256(s.buildSignature(n))
		sum = d[:]
	} else {
		d := sha1.Sum(s.buildSignature(n))
		sum = d[:]
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, h, sum)
	if err != nil {
		t.Fatal(err)
	}
	n.Signature = base64.StdEncoding.EncodeToString(sig)
	b, _ := json.Marshal(n)
	return b
}

// An SES notification is taken only from the hub's topics, signed with
// SignatureVersion 1 or 2: SNS signs any topic's messages, so a forged
// complaint from someone else's topic is refused.
func TestDenmaSESTopics(t *testing.T) {
	const hub, other = "arn:aws:sns:ca-central-1:111111111111:ses-feedback", "arn:aws:sns:ca-central-1:999999999999:forged"
	defer func(f func(string) bool) { DenmaSESTopicAllowed = f }(DenmaSESTopicAllowed)
	DenmaSESTopicAllowed = func(arn string) bool { return arn == hub }

	s, key := denmaTestSES(t)
	msg := `{"notificationType":"Complaint","mail":{"timestamp":"2026-10-16T10:00:00.000Z","destination":["Someone@Example.org"]}}`
	notif := func(topic, version string) sesNotif {
		return sesNotif{Type: "Notification", MessageId: "m1", TopicArn: topic, Message: msg,
			Timestamp: "2026-10-16T10:00:01.000Z", SignatureVersion: version}
	}

	for _, v := range []string{"1", "2"} {
		b, err := s.ProcessBounce(denmaSign(t, s, key, notif(hub, v)))
		if err != nil {
			t.Fatalf("hub's topic, version %s: %v", v, err)
		}
		if b.Email != "someone@example.org" || b.Type != "complaint" {
			t.Errorf("hub's topic, version %s: %+v", v, b)
		}
	}
	if _, err := s.ProcessBounce(denmaSign(t, s, key, notif(other, "1"))); err == nil {
		t.Error("another topic's complaint was taken")
	}
	if _, err := s.ProcessBounce(denmaSign(t, s, key, notif(hub, "3"))); err == nil {
		t.Error("an unknown signature version was taken")
	}

	// Signed for the hub's topic, then given another's.
	var n sesNotif
	_ = json.Unmarshal(denmaSign(t, s, key, notif(hub, "1")), &n)
	n.Message = `{"notificationType":"Complaint","mail":{"destination":["victim@example.org"]}}`
	b, _ := json.Marshal(n)
	if _, err := s.ProcessBounce(b); err == nil {
		t.Error("a changed message was taken")
	}

	// A subscription to another topic isn't confirmed (its URL isn't fetched).
	sub := sesNotif{Type: "SubscriptionConfirmation", MessageId: "m2", TopicArn: other, Token: "t",
		SubscribeURL: "https://127.0.0.1:1/never", Message: "confirm", Timestamp: "2026-10-16T10:00:01.000Z", SignatureVersion: "1"}
	if err := s.ProcessSubscription(denmaSign(t, s, key, sub)); err == nil || err.Error() != `SNS topic isn't the hub's: "`+other+`"` {
		t.Errorf("another topic's subscription: %v", err)
	}

	// Unset, no topic is the hub's.
	DenmaSESTopicAllowed = nil
	if _, err := s.ProcessBounce(denmaSign(t, s, key, notif(hub, "1"))); err == nil {
		t.Error("taken with no topics set")
	}
}

// A bounce or complaint is recorded against who SES says bounced or
// complained, not the e-mail's first recipient.
func TestDenmaSESRecipient(t *testing.T) {
	for msg, want := range map[string]string{
		`{"bounce":{"bouncedRecipients":[{"emailAddress":"bcc@example.org"}]},"mail":{"destination":["sub@example.org","bcc@example.org"]}}`: "bcc@example.org",
		`{"complaint":{"complainedRecipients":[{"emailAddress":"b@example.org"}]}}`:                                                          "b@example.org",
		`{"bounce":{"bouncedRecipients":[]}}`: "first@example.org",
		`not json`:                            "first@example.org",
	} {
		if got := denmaSESRecipient(msg, "first@example.org"); got != want {
			t.Errorf("%s: %q, want %q", msg, got, want)
		}
	}
}
