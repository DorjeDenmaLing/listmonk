package main

// denma: a limit on opt-in e-mails. Signing up to a double opt-in list sends
// a confirmation e-mail, and signing the same address up again sends another,
// so a public form can be used to flood someone's inbox (upstream #3229),
// here with every center's form and through the organization's one sending
// account. So an address is sent at most denmaOptinLimit opt-ins in
// denmaOptinWindow, across all centers. After that, a sign-up is told that
// one was sent already and nothing is sent (denmaCheckOptin,
// cmd/denma_emailcheck.go); the subscription is kept, unconfirmed. Admins'
// "Send opt-in e-mail" counts too.
//
// denma.optin_sends records each one by the address's SHA-256, not the
// address, and keeps only the last window's.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	denmaOptinLimit  = 5
	denmaOptinWindow = 24 * time.Hour
)

func denmaInitOptinSends(db *sqlx.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS denma.optin_sends (
		email_hash TEXT NOT NULL, -- SHA-256 of the lowercase address, hex
		center     TEXT NOT NULL, -- the slug of the center that sent it
		sent_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS optin_sends_email ON denma.optin_sends (email_hash, sent_at);
	CREATE INDEX IF NOT EXISTS optin_sends_time ON denma.optin_sends (sent_at);`)
	return err
}

// denmaOptinAllowed records an opt-in to email from a center and reports
// whether it may be sent: false once the address has had its limit. Old
// records are dropped. Without centers (no denma schema) it always may.
func denmaOptinAllowed(db *sqlx.DB, email, center string) (bool, error) {
	if denmaHub == nil {
		return true, nil
	}
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	var ok []bool
	err := db.Select(&ok, `WITH gone AS (DELETE FROM denma.optin_sends WHERE sent_at < NOW() - $4 * INTERVAL '1 second'),
		n AS (SELECT COUNT(*) AS c FROM denma.optin_sends WHERE email_hash = $1 AND sent_at >= NOW() - $4 * INTERVAL '1 second')
		INSERT INTO denma.optin_sends (email_hash, center) SELECT $1, $2 FROM n WHERE c < $3 RETURNING TRUE`,
		hex.EncodeToString(h[:]), center, denmaOptinLimit, int(denmaOptinWindow.Seconds()))
	return len(ok) > 0, err
}
