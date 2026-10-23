#!/bin/sh
# Shared local and release builds; all generated artifacts live in dist/.
set -eu

cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

mode=${1:-local}
case "$mode" in
    local|dist) ;;
    *) printf 'Usage: sh scripts/build.sh [local|dist]\n' >&2; exit 1 ;;
esac

ldflags=''
version=${VERSION:-}
if [ -n "$version" ]; then
    version=${version#v}
    if ! printf '%s\n' "$version" | LC_ALL=C grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
        printf 'VERSION must be a stable version such as 0.1.0 or v0.1.0.\n' >&2
        exit 1
    fi
    ldflags="-X github.com/ariym/mf-v2/internal/config.Version=$version"
fi

mkdir -p dist
if [ "$mode" = local ]; then
    go build -trimpath -ldflags "$ldflags" -o dist/mf ./cmd/mf
    exit 0
fi

if command -v sha256sum >/dev/null 2>&1; then
    hash_command=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    hash_command=shasum
else
    printf 'sha256sum or shasum is required to build release checksums.\n' >&2
    exit 1
fi

for platform in darwin linux; do
    for architecture in amd64 arm64; do
        asset="mf-$platform-$architecture"
        printf 'Building %s...\n' "$asset"
        CGO_ENABLED=0 GOOS="$platform" GOARCH="$architecture" go build -trimpath \
            -ldflags "-s -w $ldflags" -o "dist/$asset" ./cmd/mf
        (
            cd dist
            if [ "$hash_command" = sha256sum ]; then
                sha256sum "$asset" > "$asset.sha256"
            else
                shasum -a 256 "$asset" > "$asset.sha256"
            fi
        )
    done
done
cp scripts/install.sh dist/install.sh
