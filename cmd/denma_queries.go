package main

// denma: upstream queries replaced with the fork's, for every app (the hub's
// and each center's), from prepareQueries.

import "github.com/knadh/goyesql/v2"

// denmaQueries are the replacements, by query name.
var denmaQueries = map[string]string{
	// Maintenance's orphan clean-up keeps the blocklisted: someone whose only
	// list was deleted keeps their consent record (attribs) and why they're
	// blocklisted, as deleting all blocklisted subscribers isn't offered.
	"delete-orphan-subscribers": `DELETE FROM subscribers a WHERE a.status <> 'blocklisted' AND NOT EXISTS
    (SELECT 1 FROM subscriber_lists b WHERE b.subscriber_id = a.id)`,

	// Unique views and clicks (with individual tracking) are people's: not
	// the anonymous ones (a deleted subscriber's, or from while it was off),
	// which DISTINCT ON counted as one more person.
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
