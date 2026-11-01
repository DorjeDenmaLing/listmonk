package main

// denma: where replies go: a center's default reply-to (denma.reply_to), replaced by a campaign's own.

import (
	"errors"
	"net/mail"
	"strings"
)

// denmaReplyToSQL adds the campaign's reply-to address.
const denmaReplyToSQL = `ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_reply_to TEXT NOT NULL DEFAULT '';`

const denmaReplyToMoveSQL = `
WITH r AS (
    SELECT DISTINCT ON (c.id) c.id, kv.key AS k, TRIM(kv.value #>> '{}') AS v
    FROM campaigns c, jsonb_array_elements(CASE WHEN jsonb_typeof(c.headers) = 'array' THEN c.headers ELSE '[]' END) h,
        jsonb_each(CASE WHEN jsonb_typeof(h) = 'object' THEN h ELSE '{}' END) kv
    WHERE c.denma_reply_to = '' AND LOWER(kv.key) = 'reply-to'
)
UPDATE campaigns c SET denma_reply_to = r.v,
    headers = (SELECT COALESCE(jsonb_agg(h - r.k), '[]') FROM jsonb_array_elements(c.headers) h WHERE h - r.k <> '{}')
    FROM r WHERE c.id = r.id;
`

// denmaReplyToMax is the longest reply-to address.
const denmaReplyToMax = 300

// denmaCheckReplyTo tidies a reply-to address: "" or one address, with or without a name.
func denmaCheckReplyTo(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	if len(addr) > denmaReplyToMax {
		return "", errors.New("The reply-to address is too long.")
	}
	if _, err := mail.ParseAddress(addr); err != nil {
		return "", errors.New(`The reply-to address isn't a valid address. Use name@example.org or "Name" <name@example.org>.`)
	}
	return addr, nil
}

// denmaReplyTo is the center's default reply-to address, or "".
func (a *App) denmaReplyTo() string {
	return a.ko.String("denma.reply_to")
}
