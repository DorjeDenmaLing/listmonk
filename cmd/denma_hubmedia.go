package main

// denma: the hub's own media, for its branding (Settings -> General: its logo
// and favicon, and the admin's logo and icon). The hub's uploads folder holds
// every center's (<uploads>/<slug>), so its own media go in a folder of their
// own, <uploads>/_hub (or <bucket path>/_hub on S3), which the hub serves at
// its upload URI as a center serves its own. A slug can't have "_", so no
// center's folder is the hub's.

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/media/providers/filesystem"
	"github.com/knadh/listmonk/internal/media/providers/s3"
)

const denmaHubMediaDir = "_hub"

// denmaIsHubKo reports whether ko is the hub's, in multi-center mode.
func denmaIsHubKo(ko *koanf.Koanf) bool {
	return ko.Bool("denma.multi_center") && ko.String("denma.center") == ""
}

// denmaUploadDir is the folder an app's filesystem uploads are in and served
// from: its own, or the hub's under the hub's uploads folder.
func denmaUploadDir(ko *koanf.Koanf, dir string) string {
	if denmaIsHubKo(ko) {
		return filepath.Join(dir, denmaHubMediaDir)
	}
	return dir
}

// denmaHubMediaFS puts the hub's filesystem uploads in their own folder.
func denmaHubMediaFS(ko *koanf.Koanf, o *filesystem.Opts) {
	if !denmaIsHubKo(ko) {
		return
	}
	o.UploadPath = denmaUploadDir(ko, o.UploadPath)
	if err := os.MkdirAll(o.UploadPath, 0o755); err != nil {
		denmaLog(ko).Printf("denma: error creating the hub's uploads folder %s: %v", o.UploadPath, err)
	}
}

// denmaHubMediaS3 puts the hub's S3 uploads in their own bucket path.
func denmaHubMediaS3(ko *koanf.Koanf, o *s3.Opt) {
	if denmaIsHubKo(ko) {
		o.BucketPath = path.Join("/", o.BucketPath, denmaHubMediaDir)
	}
}

// denmaHubMediaPage reports whether p is the Media page (or its picker),
// which the hub has though it's under /admin/campaigns.
func denmaHubMediaPage(p string) bool {
	return p == "/admin/campaigns/media" || strings.HasPrefix(p, "/admin/campaigns/media/")
}

// denmaHubUpload reports whether p is under the hub's upload URI
// (filesystem), or S3's public URL when the app serves it (a path).
func denmaHubUpload(hub *App, p string) bool {
	if hub == nil {
		return false
	}
	var uri string
	switch hub.ko.String("upload.provider") {
	case "filesystem":
		uri = path.Join("/", hub.ko.String("upload.filesystem.upload_uri"))
	case "s3":
		if u := hub.ko.String("upload.s3.public_url"); strings.HasPrefix(u, "/") {
			uri = path.Clean(u)
		}
	}
	return uri != "" && uri != "/" && strings.HasPrefix(p, uri+"/")
}
