package main

// denma: invite links that survive a restart. A new center admin's
// set-password link (addCenterAdmin, cmd/denma_hub.go) is listmonk's reset
// link with a 7-day token, which listmonk keeps in memory only. Its SHA-256
// is also kept in denma.invites, and when the link is opened after a restart,
// the token is put back in memory for the rest of its time
// (denmaRestoreInvite, from listmonk's reset page). Setting the password
// deletes it (denmaInviteUsed), so a used link can't come back.

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/tmptokens"
)

func denmaInitInvites(db *sqlx.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS denma.invites (
		center_id  INTEGER NOT NULL REFERENCES denma.centers(id) ON DELETE CASCADE,
		email      TEXT NOT NULL, -- lowercase
		token_hash TEXT NOT NULL, -- SHA-256, hex
		expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
		PRIMARY KEY (center_id, email)
	)`)
	return err
}

func denmaTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// saveInvite records a center's invite (replacing an earlier one for the
// address) and drops expired ones.
func (d *denmaCenters) saveInvite(ctr *denmaCenter, email, token string, ttl time.Duration) error {
	_, err := d.base.db.Exec(`WITH gone AS (DELETE FROM denma.invites WHERE expires_at < NOW())
		INSERT INTO denma.invites (center_id, email, token_hash, expires_at) VALUES ($1, LOWER($2), $3, $4)
		ON CONFLICT (center_id, email) DO UPDATE SET token_hash = EXCLUDED.token_hash, expires_at = EXCLUDED.expires_at`,
		ctr.ID, email, denmaTokenHash(token), time.Now().Add(ttl))
	return err
}

// denmaRestoreInvite puts an invite's token back in memory if this is a
// center's unexpired invite link (after a restart, listmonk has forgotten it).
func (a *App) denmaRestoreInvite(email, token string) {
	slug := a.ko.String("denma.center")
	if denmaHub == nil || slug == "" || token == "" {
		return
	}
	var exp time.Time
	if err := a.db.Get(&exp, `SELECT i.expires_at FROM denma.invites i JOIN denma.centers c ON c.id = i.center_id
		WHERE c.slug = $1 AND i.email = LOWER($2) AND i.token_hash = $3 AND i.expires_at > NOW()`,
		slug, email, denmaTokenHash(token)); err != nil {
		return
	}
	tmptokens.Set(a.tmpKey(email), time.Until(exp), token)
}

// denmaInviteUsed forgets the address's invite once its password is set.
func (a *App) denmaInviteUsed(email string) {
	slug := a.ko.String("denma.center")
	if denmaHub == nil || slug == "" {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM denma.invites WHERE email = LOWER($2)
		AND center_id = (SELECT id FROM denma.centers WHERE slug = $1)`, slug, email); err != nil {
		a.log.Printf("denma: error removing the invite for %s: %v", email, err)
	}
}
