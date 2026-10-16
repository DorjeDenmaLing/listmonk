package webhooks

// denma: SNS signs every topic's messages, so a valid signature says only
// that AWS sent it. Anyone could subscribe their own topic to the webhook and
// send complaints for any address, so subscriptions and notifications are
// taken only from the hub's topics (cmd/denma_bounces.go).

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DenmaSESTopicAllowed reports whether an SNS topic is the hub's. Unset, none is.
var DenmaSESTopicAllowed func(arn string) bool

// denmaSNSClient fetches SNS certificates and subscription URLs.
var denmaSNSClient = &http.Client{Timeout: 10 * time.Second}

// denmaCheckNotif refuses a notification from a topic that isn't the hub's.
func denmaCheckNotif(n sesNotif) error {
	if DenmaSESTopicAllowed == nil || !DenmaSESTopicAllowed(n.TopicArn) {
		return fmt.Errorf("SNS topic isn't the hub's: %q", n.TopicArn)
	}
	return nil
}

// denmaSigAlg is the algorithm of an SNS signature version: 1 is SHA1, 2 SHA256.
func denmaSigAlg(version string) (x509.SignatureAlgorithm, error) {
	switch version {
	case "1":
		return x509.SHA1WithRSA, nil
	case "2":
		return x509.SHA256WithRSA, nil
	}
	return 0, fmt.Errorf("unknown SNS signature version %q", version)
}

// denmaSESRecipient is who bounced or complained (bouncedRecipients,
// complainedRecipients), else the e-mail's first recipient: an e-mail with
// several (a Bcc) mustn't have one's bounce recorded against another.
func denmaSESRecipient(msg, first string) string {
	var m struct {
		Bounce struct {
			To []struct {
				Email string `json:"emailAddress"`
			} `json:"bouncedRecipients"`
		} `json:"bounce"`
		Complaint struct {
			To []struct {
				Email string `json:"emailAddress"`
			} `json:"complainedRecipients"`
		} `json:"complaint"`
	}
	if json.Unmarshal([]byte(msg), &m) == nil {
		for _, to := range append(m.Bounce.To, m.Complaint.To...) {
			if to.Email != "" {
				return to.Email
			}
		}
	}
	return first
}
