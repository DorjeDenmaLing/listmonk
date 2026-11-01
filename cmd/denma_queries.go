package main

// denma: upstream queries replaced with the fork's, for every app, from prepareQueries.

import "github.com/knadh/goyesql/v2"

// denmaQueries are the replacements, by query name.
var denmaQueries = map[string]string{
	// Maintenance's orphan clean-up keeps the blocklisted.
	"delete-orphan-subscribers": `DELETE FROM subscribers a WHERE a.status <> 'blocklisted' AND NOT EXISTS
    (SELECT 1 FROM subscriber_lists b WHERE b.subscriber_id = a.id)`,

	// The opt-in page's confirmation is the person's own with their e-mail's token: marks denma.own_confirm.
	"confirm-subscription-optin": `WITH subID AS (
    SELECT id FROM subscribers WHERE uuid = $1::UUID
),
listIDs AS (
    SELECT id FROM lists WHERE uuid = ANY($2::UUID[])
)
UPDATE subscriber_lists SET status='confirmed', meta=meta || ($3::JSONB - '` + denmaOwnConfirmKey + `'), updated_at=NOW()
    WHERE subscriber_id = (SELECT id FROM subID) AND list_id = ANY(SELECT id FROM listIDs)
    AND (SELECT set_config('denma.own_confirm', CASE WHEN $3::JSONB ? '` + denmaOwnConfirmKey + `' THEN 'on' ELSE '' END, true)) IS NOT NULL;`,

	// A blocklist import gives the triggers the file's dates (denma.import_file, cmd/denma_imports.go).
	"upsert-blocklist-subscriber": `WITH sub AS (
    INSERT INTO subscribers (uuid, email, name, attribs, status)
    VALUES($1, $2, $3, $4, 'blocklisted')
    ON CONFLICT (email) DO UPDATE SET status='blocklisted', updated_at=NOW()
        WHERE set_config('denma.import_file', jsonb_build_object('unsubscribed_at', EXCLUDED.attribs->'unsubscribed_at',
            'bounced_at', EXCLUDED.attribs->'bounced_at')::TEXT, true) IS NOT NULL
    RETURNING id
)
UPDATE subscriber_lists SET status='unsubscribed', updated_at=NOW()
    WHERE subscriber_id = (SELECT id FROM sub);`,

	// Unique views and clicks are people's, not the anonymous ones.
	"get-campaign-analytics-unique-counts": `WITH intval AS (
    SELECT CASE WHEN (EXTRACT (EPOCH FROM ($3::TIMESTAMP - $2::TIMESTAMP)) / 86400) >= 7 THEN 'day' ELSE 'hour' END
),
uniqIDs AS (
    SELECT DISTINCT ON(subscriber_id, campaign_id) subscriber_id, campaign_id, DATE_TRUNC((SELECT * FROM intval), created_at) AS "timestamp"
    FROM %s
    WHERE campaign_id=ANY($1) AND created_at >= $2 AND created_at <= $3 AND subscriber_id IS NOT NULL
    ORDER BY subscriber_id, campaign_id, "timestamp"
)
SELECT COUNT(*) AS "count", campaign_id, "timestamp"
    FROM uniqIDs GROUP BY campaign_id, "timestamp" ORDER BY "timestamp" ASC;`,
}

// denmaReplaceQueries puts the fork's queries in qMap.
func denmaReplaceQueries(qMap goyesql.Queries) {
	for name, q := range denmaQueries {
		if old, ok := qMap[name]; ok {
			old.Query = q
		}
	}
}
