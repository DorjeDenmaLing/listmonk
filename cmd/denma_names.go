package main

// denma: subscribers' first and last names; the name is the two joined, kept by denma_keep_names.

import (
	"strings"

	"github.com/knadh/listmonk/models"
)

// denmaNameMax is the most characters of a first or last name.
const denmaNameMax = 100

// denmaNamesColumnsSQL adds the first and last name columns (denmaNamesSQL, denmaCampaignColumnsSQL).
const denmaNamesColumnsSQL = `ALTER TABLE subscribers ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE subscribers ADD COLUMN IF NOT EXISTS last_name TEXT NOT NULL DEFAULT '';`

const denmaNamesSQL = denmaNamesColumnsSQL + `

-- Spaces collapsed and trimmed.
CREATE OR REPLACE FUNCTION denma_tidy_name(n TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $$ SELECT btrim(regexp_replace(COALESCE(n, ''), '\s+', ' ', 'g')) $$;

-- A name split once at the first space; a stand-in (the address, or its local part) has none.
CREATE OR REPLACE FUNCTION denma_split_name(name TEXT, email TEXT) RETURNS TEXT[]
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN n = '' OR lower(n) IN (lower(btrim(email)), lower(split_part(btrim(email), '@', 1)),
            lower(denma_tidy_name(replace(split_part(btrim(email), '@', 1), '.', ' ')))) THEN ARRAY['', '']
        ELSE ARRAY[substring(n FROM '^\S+'), regexp_replace(n, '^\S+ ?', '')] END
    FROM (SELECT denma_tidy_name(name) AS n) x;
$$;

-- The name is the two names joined, or a given name split; names come as attribs.denma_first_name/_last_name.
CREATE OR REPLACE FUNCTION denma_keep_names() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    parts TEXT[];
BEGIN
    IF jsonb_typeof(NEW.attribs) = 'object' AND (NEW.attribs ? 'denma_first_name' OR NEW.attribs ? 'denma_last_name') THEN
        NEW.first_name := COALESCE(NEW.attribs->>'denma_first_name', '');
        NEW.last_name := COALESCE(NEW.attribs->>'denma_last_name', '');
        NEW.attribs := NEW.attribs - 'denma_first_name' - 'denma_last_name';
    END IF;
    NEW.first_name := left(denma_tidy_name(NEW.first_name), 100);
    NEW.last_name := left(denma_tidy_name(NEW.last_name), 100);
    IF TG_OP = 'INSERT' OR NEW.first_name IS DISTINCT FROM OLD.first_name OR NEW.last_name IS DISTINCT FROM OLD.last_name THEN
        IF NEW.first_name <> '' OR NEW.last_name <> '' THEN
            NEW.name := btrim(NEW.first_name || ' ' || NEW.last_name);
            RETURN NEW;
        END IF;
        IF TG_OP = 'UPDATE' THEN
            RETURN NEW; -- both cleared: the name stays
        END IF;
    ELSIF NEW.name IS NOT DISTINCT FROM OLD.name THEN
        RETURN NEW;
    END IF;
    NEW.name := denma_tidy_name(NEW.name);
    parts := denma_split_name(NEW.name, NEW.email);
    NEW.first_name := left(parts[1], 100);
    NEW.last_name := left(parts[2], 100);
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_names ON subscribers;
CREATE TRIGGER denma_keep_names BEFORE INSERT OR UPDATE OF name, first_name, last_name, attribs ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_keep_names();
`

// denmaSplitNamesSQL splits the existing names once.
const denmaSplitNamesSQL = `UPDATE subscribers s SET first_name = p[1], last_name = p[2]
	FROM (SELECT id, denma_split_name(name, email) AS p FROM subscribers WHERE first_name = '' AND last_name = '') x
	WHERE s.id = x.id AND (p[1] <> '' OR p[2] <> '')`

// denmaTidyName tidies a name as the database does: spaces collapsed, at most denmaNameMax characters.
func denmaTidyName(n string) string {
	n = strings.Join(strings.Fields(reDenmaControl.ReplaceAllString(n, " ")), " ")
	if r := []rune(n); len(r) > denmaNameMax {
		n = strings.TrimSpace(string(r[:denmaNameMax]))
	}
	return n
}

// denmaJoinName is the name of a first and last name ("" for neither).
func denmaJoinName(first, last string) string {
	return strings.TrimSpace(denmaTidyName(first) + " " + denmaTidyName(last))
}

// denmaSplitName splits a name as denma_split_name does: once at the first space; none for a stand-in.
func denmaSplitName(name, email string) (string, string) {
	n := strings.Join(strings.Fields(name), " ")
	local, _, _ := strings.Cut(strings.TrimSpace(email), "@")
	if n == "" || strings.EqualFold(n, strings.TrimSpace(email)) || strings.EqualFold(n, local) ||
		strings.EqualFold(n, strings.Join(strings.Fields(strings.ReplaceAll(local, ".", " ")), " ")) {
		return "", ""
	}
	first, last, _ := strings.Cut(n, " ")
	return denmaTidyName(first), denmaTidyName(last)
}

// denmaWithNames is attribs with first and last names for denma_keep_names to take.
func denmaWithNames(attribs map[string]any, first, last string) map[string]any {
	if attribs == nil {
		attribs = map[string]any{}
	}
	attribs["denma_first_name"], attribs["denma_last_name"] = denmaTidyName(first), denmaTidyName(last)
	return attribs
}

// denmaFormName is a public page's name: first and last joined if given, else name.
func denmaFormName(name, first, last string) string {
	if n := denmaJoinName(first, last); n != "" {
		return n
	}
	return name
}

// denmaFormNames is the attributes giving a new subscriber a form's first and last names, or none.
func denmaFormNames(first, last string) models.JSON {
	if denmaJoinName(first, last) == "" {
		return nil
	}
	return denmaWithNames(nil, first, last)
}

// denmaPrefsNames gives sub the preferences page's first and last names.
func denmaPrefsNames(sub *models.Subscriber, first, last string) {
	sub.DenmaFirstName, sub.DenmaLastName = first, last
	denmaNamesIn(sub, nil)
}

// denmaNamesIn makes s's name from any first and last names given; before is the stored subscriber.
func denmaNamesIn(s *models.Subscriber, before *models.Subscriber) {
	first, last := denmaTidyName(s.DenmaFirstName), denmaTidyName(s.DenmaLastName)
	if before != nil && first == before.DenmaFirstName && last == before.DenmaLastName {
		return
	}
	if first == "" && last == "" && before == nil {
		return
	}
	if n := denmaJoinName(first, last); n != "" {
		s.Name = n
	}
	s.Attribs = denmaWithNames(s.Attribs, first, last)
}
