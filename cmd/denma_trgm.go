package main

// denma: trigram indexes on subscribers' names and addresses, for the
// search's "contains" matches (ILIKE '%x%'). pg_trgm is made once, before the
// centers load; a center loaded without it has no such indexes.

import "github.com/jmoiron/sqlx"

// denmaInitTrigram makes pg_trgm if it can.
func denmaInitTrigram(db *sqlx.DB) {
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public`); err != nil {
		lo.Printf("denma: pg_trgm not available, so searches by name or address aren't indexed: %v", err)
	}
}

// denmaTrigramSQL makes a center's indexes (denmaFeaturesSQL).
const denmaTrigramSQL = `
DO $$
DECLARE ns TEXT;
BEGIN
    SELECT n.nspname INTO ns FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm';
    IF ns IS NOT NULL THEN
        EXECUTE format('CREATE INDEX IF NOT EXISTS denma_subscribers_name_trgm ON subscribers USING GIN (name %I.gin_trgm_ops)', ns);
        EXECUTE format('CREATE INDEX IF NOT EXISTS denma_subscribers_email_trgm ON subscribers USING GIN (email %I.gin_trgm_ops)', ns);
    END IF;
END $$;
`
