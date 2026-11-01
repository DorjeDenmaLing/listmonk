package main

// denma: an opt-in e-mail's link carries a token of its own (?t=), kept hashed for denmaResubTTL.

import (
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
)

// denmaOptinTokenSQL makes the tokens' table, in a center (denmaFeaturesSQL).
const denmaOptinTokenSQL = `
CREATE TABLE IF NOT EXISTS denma_optin_tokens (
    token_hash    TEXT PRIMARY KEY,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_optin_tokens_subscriber ON denma_optin_tokens (subscriber_id);
`

// denmaOptinSending holds the token of the opt-in e-mail being made, by subscriber UUID.
var denmaOptinSending sync.Map

// denmaWithOptinToken makes a token for the opt-in e-mail send makes.
func denmaWithOptinToken(db *sqlx.DB, sub models.Subscriber, send func() (int, error)) (int, error) {
	tok, err := utils.GenerateRandomString(32)
	if err == nil {
		_, err = db.Exec(`WITH old AS (DELETE FROM denma_optin_tokens WHERE created_at < NOW() - $3 * INTERVAL '1 second')
			INSERT INTO denma_optin_tokens (token_hash, subscriber_id) VALUES ($1, $2)`,
			denmaTokenHash(tok), sub.ID, int(denmaResubTTL.Seconds()))
	}
	if err != nil {
		lo.Printf("denma: error making subscriber %d's opt-in token: %v", sub.ID, err)
		return send()
	}
	denmaOptinSending.Store(sub.UUID, tok)
	defer denmaOptinSending.Delete(sub.UUID)
	return send()
}

// denmaOptinTokenURL adds the token of the e-mail being made to its opt-in URL.
func denmaOptinTokenURL(u, subUUID string) string {
	tok, ok := denmaOptinSending.Load(subUUID)
	if !ok {
		return u
	}
	if !strings.HasSuffix(u, "?") {
		u += "&"
	}
	return u + "t=" + tok.(string)
}

// denmaOptinTokenOK reports whether a token is one of a subscriber's opt-in e-mails', and unexpired.
// denmaOwnConfirmKey marks an opt-in page confirmation's meta as the person's own.
const denmaOwnConfirmKey = "denma_own_confirm"

// denmaOwnConfirm marks meta for the subscriber's confirmation as their own, if it has their token.
func (a *App) denmaOwnConfirm(meta models.JSON, subUUID, token string) {
	var subID int
	if err := a.db.Get(&subID, `SELECT id FROM subscribers WHERE uuid = $1`, subUUID); err == nil && denmaOptinTokenOK(a.db, subID, token) {
		meta[denmaOwnConfirmKey] = true
	}
}

func denmaOptinTokenOK(db *sqlx.DB, subID int, token string) bool {
	if token == "" {
		return false
	}
	var ok bool
	if err := db.Get(&ok, `SELECT EXISTS (SELECT 1 FROM denma_optin_tokens WHERE token_hash = $1 AND subscriber_id = $2
		AND created_at > NOW() - $3 * INTERVAL '1 second')`, denmaTokenHash(token), subID, int(denmaResubTTL.Seconds())); err != nil {
		lo.Printf("denma: error checking subscriber %d's opt-in token: %v", subID, err)
		return false
	}
	return ok
}
