package main

// denma: every center has the same layout, DDL included: its data in the
// schema center_<slug>, and its media in <uploads>/<slug> (filesystem) or
// <bucket path>/<slug> (S3), served at /c/<slug>/uploads. A center that's
// somewhere else, such as an existing install registered as a center (DDL's,
// in public), is moved when it loads:
//
//   - ownSchema, before anything opens it: its tables (with their indexes,
//     constraints and sequences), views, materialized views, sequences, types
//     and functions, in one transaction, which also registers the new schema.
//     Functions owned by another role (e.g. triggers installed by a superuser)
//     stay where they are and still work: triggers move with their tables, and
//     the functions find tables by the center's search path. Anything else it
//     can't move stops the move (and the center's loading), changing nothing.
//   - ownUploads: its media files are copied to its folder, the setting is
//     changed, and then the old files are deleted.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/lib/pq"
)

// denmaSchemaName is a center's schema.
func denmaSchemaName(slug string) string {
	return "center_" + strings.ReplaceAll(slug, "-", "_")
}

// denmaUploadPaths are a center's uploads folder and S3 bucket path, under
// the hub's.
func denmaUploadPaths(base *koanf.Koanf, slug string) (string, string) {
	uploads := strings.TrimSuffix(base.String("upload.filesystem.upload_path"), "/")
	if uploads == "" {
		uploads = "uploads"
	}
	return path.Join(uploads, slug), path.Join("/", base.String("upload.s3.bucket_path"), slug)
}

// ownSchema moves a center into its own schema if it's elsewhere.
func (d *denmaCenters) ownSchema(c *denmaCenter) error {
	want := denmaSchemaName(c.Slug)
	if c.Schema == want {
		return nil
	}
	db := d.current().db

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var tables struct {
		Old int `db:"old"`
		New int `db:"new"`
	}
	if err := tx.Get(&tables, `SELECT
		(SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'r') AS old,
		(SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $2 AND c.relkind = 'r') AS new`, c.Schema, want); err != nil {
		return err
	}
	switch {
	case tables.New > 0 && tables.Old > 0:
		return fmt.Errorf("can't move %s from %s to %s: both have tables", c.Slug, c.Schema, want)
	case tables.Old == 0:
		// Nothing to move (a center not yet provisioned, or already moved).
		if _, err := tx.Exec(`UPDATE denma.centers SET schema_name = $1 WHERE id = $2`, want, c.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		c.Schema = want
		return nil
	}

	lo.Printf("denma: moving center %s from schema %s to %s", c.Slug, c.Schema, want)
	if _, err := tx.Exec(`CREATE SCHEMA IF NOT EXISTS ` + pq.QuoteIdentifier(want)); err != nil {
		return fmt.Errorf("creating schema %s: %v", want, err)
	}

	// What's in the schema, but not part of an extension (pgcrypto) or of
	// something else that moves (a table's sequences, its row type, an
	// enum's array type).
	var objs []struct {
		Kind  string `db:"kind"`
		Name  string `db:"name"`
		Owned bool   `db:"owned"`
	}
	if err := tx.Select(&objs, `
		WITH ns AS (SELECT oid FROM pg_namespace WHERE nspname = $1)
		SELECT CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' WHEN 'v' THEN 'VIEW'
				WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' WHEN 'c' THEN 'TYPE' END AS kind,
			FORMAT('%I.%I', $1::TEXT, c.relname) AS name, pg_has_role(c.relowner, 'USAGE') AS owned
			FROM pg_class c
			WHERE c.relnamespace = (SELECT oid FROM ns) AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f', 'c') AND NOT c.relispartition
			AND NOT EXISTS (SELECT 1 FROM pg_depend dp WHERE dp.classid = 'pg_class'::REGCLASS AND dp.objid = c.oid AND dp.deptype IN ('e', 'a', 'i'))
		UNION ALL
		SELECT 'TYPE', FORMAT('%I.%I', $1::TEXT, t.typname), pg_has_role(t.typowner, 'USAGE')
			FROM pg_type t
			WHERE t.typnamespace = (SELECT oid FROM ns) AND t.typtype IN ('e', 'd', 'r')
			AND NOT EXISTS (SELECT 1 FROM pg_depend dp WHERE dp.classid = 'pg_type'::REGCLASS AND dp.objid = t.oid AND dp.deptype = 'e')
		UNION ALL
		SELECT 'ROUTINE', p.oid::REGPROCEDURE::TEXT, pg_has_role(p.proowner, 'USAGE')
			FROM pg_proc p
			WHERE p.pronamespace = (SELECT oid FROM ns)
			AND NOT EXISTS (SELECT 1 FROM pg_depend dp WHERE dp.classid = 'pg_proc'::REGCLASS AND dp.objid = p.oid AND dp.deptype = 'e')`, c.Schema); err != nil {
		return fmt.Errorf("listing %s's objects: %v", c.Schema, err)
	}

	// Functions first: their names include their argument types' schema.
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Kind == "ROUTINE" && objs[j].Kind != "ROUTINE" })

	moved, left := 0, []string{}
	for _, o := range objs {
		if !o.Owned {
			if o.Kind == "ROUTINE" {
				left = append(left, o.Name)
				continue
			}
			return fmt.Errorf("can't move %s to %s: %s %s belongs to another database role", c.Slug, want, strings.ToLower(o.Kind), o.Name)
		}
		if _, err := tx.Exec(`ALTER ` + o.Kind + ` ` + o.Name + ` SET SCHEMA ` + pq.QuoteIdentifier(want)); err != nil {
			return fmt.Errorf("moving %s %s to %s: %v", strings.ToLower(o.Kind), o.Name, want, err)
		}
		moved++
	}

	// listmonk's pgcrypto functions, as in every center's schema.
	if _, err := tx.Exec(`SET LOCAL search_path TO ` + pq.QuoteIdentifier(want)); err != nil {
		return err
	}
	if err := denmaCryptoFuncs(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE denma.centers SET schema_name = $1 WHERE id = $2`, want, c.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moving %s to %s: %v", c.Slug, want, err)
	}

	lo.Printf("denma: moved center %s to schema %s (%d objects)", c.Slug, want, moved)
	if len(left) > 0 {
		lo.Printf("denma: left in %s, owned by another database role (they still work): %s", c.Schema, strings.Join(left, ", "))
	}
	c.Schema = want
	return nil
}

// ownUploads moves a center's media to its own uploads folder (or S3 bucket
// path) if they're elsewhere, and sets both.
func (d *denmaCenters) ownUploads(c *denmaCenter, db *sqlx.DB) error {
	base := d.base.ko
	wantFS, wantS3 := denmaUploadPaths(base, c.Slug)
	want := map[string]string{"upload.filesystem.upload_path": wantFS, "upload.s3.bucket_path": wantS3}

	cur := map[string]string{}
	for k := range want {
		var raw []byte
		if err := db.Get(&raw, `SELECT value FROM settings WHERE key = $1`, k); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var v string
		_ = json.Unmarshal(raw, &v)
		cur[k] = v
	}

	// The files, for the provider in use.
	provider := base.String("upload.provider")
	key := map[string]string{"filesystem": "upload.filesystem.upload_path", "s3": "upload.s3.bucket_path"}[provider]
	var old []string
	if key != "" && cur[key] != "" && path.Clean(cur[key]) != path.Clean(want[key]) {
		var err error
		if old, err = d.copyMedia(c, db, provider, key, cur[key], want[key]); err != nil {
			return err
		}
	}

	for k, v := range want {
		if cur[k] == v {
			continue
		}
		if _, err := db.Exec(`UPDATE settings SET value = to_jsonb($1::TEXT), updated_at = NOW() WHERE key = $2`, v, k); err != nil {
			return fmt.Errorf("setting %s: %v", k, err)
		}
	}

	// Only now that the center uses the new place.
	if len(old) > 0 {
		store := initMediaStore(denmaUploadsConfig(base, key, cur[key]))
		for _, name := range old {
			_ = store.Delete(name)
		}
		lo.Printf("denma: moved %s's media from %s to %s (%d files)", c.Slug, cur[key], want[key], len(old))
	}
	return nil
}

// copyMedia copies a center's media files (and their thumbnails) from one
// uploads folder or bucket path to another. It returns the files copied.
func (d *denmaCenters) copyMedia(c *denmaCenter, db *sqlx.DB, provider, key, from, to string) ([]string, error) {
	var files []struct {
		Name  string `db:"filename"`
		CType string `db:"content_type"`
	}
	if err := db.Select(&files, `SELECT filename, content_type FROM media WHERE provider = $1`, provider); err != nil {
		return nil, fmt.Errorf("listing %s's media: %v", c.Slug, err)
	}
	if len(files) == 0 {
		return nil, nil
	}
	if provider == "filesystem" {
		if err := os.MkdirAll(to, 0o755); err != nil {
			return nil, err
		}
	}

	var (
		src    = initMediaStore(denmaUploadsConfig(d.base.ko, key, from))
		dst    = initMediaStore(denmaUploadsConfig(d.base.ko, key, to))
		copied []string
	)
	for _, f := range files {
		for i, name := range []string{f.Name, thumbPrefix + f.Name} {
			b, err := src.GetBlob(name)
			if err != nil {
				if _, err2 := dst.GetBlob(name); i == 1 || err2 == nil {
					continue // no thumbnail, or already copied
				}
				lo.Printf("denma: %s's media file %s is missing from %s: %v", c.Slug, name, from, err)
				continue
			}
			if _, err := dst.Put(name, f.CType, bytes.NewReader(b)); err != nil {
				return nil, fmt.Errorf("copying %s's media file %s to %s: %v", c.Slug, name, to, err)
			}
			copied = append(copied, name)
		}
	}
	return copied, nil
}

// denmaUploadsConfig is the hub's config with another uploads folder or
// bucket path, for a media store.
func denmaUploadsConfig(base *koanf.Koanf, key, value string) *koanf.Koanf {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(base.All(), "."), nil)
	_ = k.Set(key, value)
	return k
}
