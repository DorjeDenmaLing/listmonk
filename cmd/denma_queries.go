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
}

// denmaReplaceQueries puts the fork's queries in qMap.
func denmaReplaceQueries(qMap goyesql.Queries) {
	for name, q := range denmaQueries {
		if old, ok := qMap[name]; ok {
			old.Query = q
		}
	}
}
