package main

// denma: every center has the same layout: its data in the schema
// center_<slug>, and its media in <uploads>/<slug> (filesystem) or
// <bucket path>/<slug> (S3), served at /c/<slug>/uploads. The hub's own media
// are in <uploads>/_hub (cmd/denma_hubmedia.go).

import (
	"path"
	"strings"

	"github.com/knadh/koanf/v2"
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
