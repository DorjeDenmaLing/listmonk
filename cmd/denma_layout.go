package main

// denma: a center's data is in schema center_<slug>, its media in <uploads>/<slug>.

import (
	"path"
	"strings"

	"github.com/knadh/koanf/v2"
)

// denmaSchemaName is a center's schema.
func denmaSchemaName(slug string) string {
	return "center_" + strings.ReplaceAll(slug, "-", "_")
}

// denmaUploadPaths are a center's uploads folder and S3 bucket path, under the hub's.
func denmaUploadPaths(base *koanf.Koanf, slug string) (string, string) {
	uploads := strings.TrimSuffix(base.String("upload.filesystem.upload_path"), "/")
	if uploads == "" {
		uploads = "uploads"
	}
	return path.Join(uploads, slug), path.Join("/", base.String("upload.s3.bucket_path"), slug)
}
