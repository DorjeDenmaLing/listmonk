package main

// denma: the limit on opt-in e-mails an address is sent across all centers (denma.optin_sends).

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

// denmaOptinAllowed records an opt-in to email from a center and reports whether it may be sent.
func denmaOptinAllowed(db *sqlx.DB, email, center string) (bool, error) {
	if denmaHub == nil {
		return true, nil
	}
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('denma_optin/' || $1))`, hex.EncodeToString(h[:])); err != nil {
		return false, err
	}
	var ok []bool
	err = tx.Select(&ok, `WITH gone AS (DELETE FROM denma.optin_sends WHERE sent_at < NOW() - $4 * INTERVAL '1 second'),
		n AS (SELECT COUNT(*) AS c FROM denma.optin_sends WHERE email_hash = $1 AND sent_at >= NOW() - $4 * INTERVAL '1 second')
		INSERT INTO denma.optin_sends (email_hash, center) SELECT $1, $2 FROM n WHERE c < $3 RETURNING TRUE`,
		hex.EncodeToString(h[:]), center, denmaOptinLimit, int(denmaOptinWindow.Seconds()))
	if err != nil {
		return false, err
	}
	return len(ok) > 0, tx.Commit()
}
