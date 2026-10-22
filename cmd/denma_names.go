package main

// denma: subscribers' first and last names. A subscriber has first_name and
// last_name, and their name (listmonk's, which its pages, search and exports
// read) is the two joined, kept so by a trigger (denma_keep_names) however
// they're set. Either can be blank (a single name).
//
//   - Given first and last names (the admin, imports, sign-up webhooks, the
//     sending API, the public pages), the name is made from them.
//   - Given only a name (listmonk's own paths, or a request without them),
//     it's split once at the first space: "Jane Mary Smith" is Jane and
//     Mary Smith. So were the names subscribers had before (features
//     version 35).
//   - A stand-in name (their address, the part before the @, or the
//     importer's "Jane Doe" from jane.doe@) has no first or last name.
//
// Templates' .Subscriber.FirstName and .LastName give them (models/
// subscribers.go), and listmonk's guess from the name only when neither is
// set.

import (
	"strings"

	"github.com/knadh/listmonk/models"
)

// denmaNameMax is the most characters of a first or last name (as
// denma_keep_names keeps them).
const denmaNameMax = 100

// denmaNamesColumnsSQL adds the columns, which listmonk's export queries
// read: in a center (denmaNamesSQL), on a new install and in the hub
// (denmaCampaignColumnsSQL, before the queries are prepared).
const denmaNamesColumnsSQL = `ALTER TABLE subscribers ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE subscribers ADD COLUMN IF NOT EXISTS last_name TEXT NOT NULL DEFAULT '';`

const denmaNamesSQL = denmaNamesColumnsSQL + `

-- Spaces collapsed and trimmed.
CREATE OR REPLACE FUNCTION denma_tidy_name(n TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $$ SELECT btrim(regexp_replace(COALESCE(n, ''), '\s+', ' ', 'g')) $$;

-- A name split once at the first space; a stand-in (the address, the part
-- before the @, or that with its dots as spaces, as the importer makes it)
-- isn't one.
CREATE OR REPLACE FUNCTION denma_split_name(name TEXT, email TEXT) RETURNS TEXT[]
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN n = '' OR lower(n) IN (lower(btrim(email)), lower(split_part(btrim(email), '@', 1)),
            lower(denma_tidy_name(replace(split_part(btrim(email), '@', 1), '.', ' ')))) THEN ARRAY['', '']
        ELSE ARRAY[substring(n FROM '^\S+'), regexp_replace(n, '^\S+ ?', '')] END
    FROM (SELECT denma_tidy_name(name) AS n) x;
$$;

-- The name is the first and last names joined; given only a name, it's
-- split. listmonk's own statements, which write only the name, are given
-- them as attribs.denma_first_name and denma_last_name (denmaWithNames, and
-- the importer's internal/subimporter/denma.go), taken out here (before
-- denma_mark_imported_subscriber, by the triggers' names). Requests can't
-- give attributes starting denma_ (denmaReservedAttribs).
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

// denmaSplitNamesSQL splits the names subscribers had before (features
// version 35), once.
const denmaSplitNamesSQL = `UPDATE subscribers s SET first_name = p[1], last_name = p[2]
	FROM (SELECT id, denma_split_name(name, email) AS p FROM subscribers WHERE first_name = '' AND last_name = '') x
	WHERE s.id = x.id AND (p[1] <> '' OR p[2] <> '')`

// denmaTidyName is a first or last name as the database keeps it: spaces
// collapsed and trimmed, at most denmaNameMax characters.
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

// denmaSplitName is a name split as the database splits it
// (denma_split_name): once, at the first space; none for a stand-in.
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

// denmaWithNames is attribs with first and last names for the database to
// take (denma_keep_names) as listmonk's statement saves the subscriber.
func denmaWithNames(attribs map[string]any, first, last string) map[string]any {
	if attribs == nil {
		attribs = map[string]any{}
	}
	attribs["denma_first_name"], attribs["denma_last_name"] = denmaTidyName(first), denmaTidyName(last)
	return attribs
}

// denmaFormName is a public page's name: its first and last names joined,
// if given, else the name.
func denmaFormName(name, first, last string) string {
	if n := denmaJoinName(first, last); n != "" {
		return n
	}
	return name
}

// denmaFormNames is the attributes that give a new subscriber a public
// form's first and last names (denmaWithNames), or none.
func denmaFormNames(first, last string) models.JSON {
	if denmaJoinName(first, last) == "" {
		return nil
	}
	return denmaWithNames(nil, first, last)
}

// denmaPrefsNames gives sub the preferences page's first and last names, for
// listmonk to save.
func denmaPrefsNames(sub *models.Subscriber, first, last string) {
	sub.DenmaFirstName, sub.DenmaLastName = first, last
	denmaNamesIn(sub, nil)
}

// denmaNamesIn readies an admin's request (s) for listmonk to save, after
// its attributes are checked: given first or last names, its name is made
// from them, and they go with its attributes. before is the subscriber as
// stored, for a PATCH (which starts from it): if they're unchanged, nothing
// is added, so a new name alone is split by the database. Given neither, so
// is the name.
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
