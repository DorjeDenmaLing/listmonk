#!/bin/sh
# denma: builds the fork as a release does, for the workflows
# (denma-ci.yml, denma-deploy.yml): the admin assets (Bun), the Go binary for
# linux/<goarch> (amd64 by default), then bundles the templates and assets into
# it with stuffbin (Makefile STATIC). Dev builds the same way (docker/start.sh
# in the project folder).
#   .github/denma-build.sh <output> [goarch]
#   .github/denma-build.sh --stuff <binary>   only bundle (a test binary)
set -eu

stuff() {
    go run github.com/knadh/stuffbin/stuffbin -a stuff -in "$1" -out "$1" \
        config.toml.sample schema.sql queries:/queries permissions.json \
        static/public:/public static/admin/views:/admin/views static/admin/partials:/admin/partials \
        static/admin/dist:/admin/static static/admin/i18n.txt:/admin/i18n.txt \
        static/email-templates i18n:/i18n
}

if [ "$1" = --stuff ]; then
    stuff "$2"
    exit
fi
OUT=$1
ARCH=${2:-amd64}

(cd static/admin && bun install --frozen-lockfile && bun run build)

# "(ddl <commit> ...)": the deploy command (listmonk-github-deploy.yaml) only
# goes back to a build whose --version says so.
VERSION=$(grep -oE '^v[0-9.]+' VERSION || git describe --tags --abbrev=0)
COMMIT=$(git rev-parse --short HEAD)
BUILDSTR="$VERSION (ddl $COMMIT $(date -u +%Y-%m-%dT%H:%M:%SZ))"
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -o "$OUT" \
    -ldflags "-s -w -X 'main.buildString=$BUILDSTR' -X 'main.versionString=$VERSION'" ./cmd
stuff "$OUT"
echo "Built $OUT: $BUILDSTR, linux/$ARCH"
