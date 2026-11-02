#!/bin/sh
# denma: installs the Go and Bun that builds use (listmonk-infra's CodeBuild
# project), each checked against its checksum. A new .go-version needs its
# version and checksums here too (go.dev/dl/?mode=json).
#   .github/denma-tools.sh <dir>   then PATH=<dir>/go/bin:<dir>/bin:$PATH
set -eu

GO_VERSION=1.26.10
GO_SHA256_arm64=3f4227ce7b206a513e794f1393adb473a07b46e94d18bd15d1c684bbb7319797
GO_SHA256_amd64=ccfe7ae822d2c2f41f4fe7c956199967a4dac22d0ee0d2b2bfdb440aab3b20c5
BUN_VERSION=1.4.4
BUN_SHA256_arm64=baa6bb36f51d6e58561e02bbea825038a9df454f673d6d776e8bbf86f8c7850e
BUN_SHA256_amd64=bf8ccbfb2b0b80159e0f9a6d15c55317add8077534dbc6130a3edb52fccb395c

DIR=$1
case "$(uname -m)" in
aarch64 | arm64) ARCH=arm64 BUN_ARCH=aarch64 go_sum=$GO_SHA256_arm64 bun_sum=$BUN_SHA256_arm64 ;;
x86_64 | amd64) ARCH=amd64 BUN_ARCH=x64 go_sum=$GO_SHA256_amd64 bun_sum=$BUN_SHA256_amd64 ;;
*) echo "No tools for $(uname -m)" >&2; exit 1 ;;
esac
if [ "$(cat "$(dirname "$0")/../.go-version")" != "$GO_VERSION" ]; then
    echo ".go-version isn't $GO_VERSION: update the version and checksums in $0" >&2
    exit 1
fi

# fetch URL SHA256 FILE
fetch() {
    curl -fsSL --retry 3 -o "$3" "$1"
    echo "$2  $3" | sha256sum -c --quiet -
}

mkdir -p "$DIR/bin"
fetch "https://go.dev/dl/go$GO_VERSION.linux-$ARCH.tar.gz" "$go_sum" "$DIR/go.tgz"
rm -rf "$DIR/go"
tar -xzf "$DIR/go.tgz" -C "$DIR"
fetch "https://github.com/oven-sh/bun/releases/download/bun-v$BUN_VERSION/bun-linux-$BUN_ARCH.zip" "$bun_sum" "$DIR/bun.zip"
unzip -q -o -j "$DIR/bun.zip" "bun-linux-$BUN_ARCH/bun" -d "$DIR/bin"
rm -f "$DIR/go.tgz" "$DIR/bun.zip"
echo "Go $("$DIR/go/bin/go" env GOVERSION), Bun $("$DIR/bin/bun" --version), linux/$ARCH"
