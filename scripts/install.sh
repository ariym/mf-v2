#!/bin/sh
# Install the latest stable GitHub release, or the tag selected by MF_VERSION.
set -eu

main() {
    case "$(uname -s)" in
        Darwin) platform=darwin ;;
        Linux) platform=linux ;;
        *) fail "Unsupported operating system: $(uname -s). mf supports macOS and Linux." ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) architecture=amd64 ;;
        arm64|aarch64) architecture=arm64 ;;
        *) fail "Unsupported architecture: $(uname -m). mf supports amd64 and arm64." ;;
    esac
    for command in curl python3; do
        command -v "$command" >/dev/null 2>&1 || fail "$command is required to install mf."
    done
    if command -v sha256sum >/dev/null 2>&1; then
        hash_command=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        hash_command=shasum
    else
        fail "sha256sum or shasum is required to verify the download."
    fi

    repository=ariym/mf-v2
    asset="mf-$platform-$architecture"
    release_version=${MF_VERSION:-latest}
    if [ "$release_version" != latest ]; then
        python3 -c 'import re, sys; sys.exit(0 if re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", sys.argv[1]) else "mf installer: MF_VERSION must be a stable tag such as v0.1.0.")' "$release_version"
    fi
    GH_TOKEN=${GH_TOKEN:-${GITHUB_TOKEN:-}}
    if [ -z "$GH_TOKEN" ] && command -v gh >/dev/null 2>&1; then
        GH_TOKEN=$(gh auth token --hostname github.com 2>/dev/null || true)
    fi
    install_dir=${MF_INSTALL_DIR:-"${HOME:?HOME must be set}/.local/bin"}
    temporary_dir=''
    staged_file=''
    trap cleanup 0
    trap 'exit 130' INT
    trap 'exit 143' TERM
    temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/mf-install.XXXXXX")

    release_endpoint=latest
    if [ "$release_version" != latest ]; then release_endpoint="tags/$release_version"; fi
    printf 'Finding release %s for %s/%s...\n' "$release_version" "$platform" "$architecture"
    if ! download "https://api.github.com/repos/$repository/releases/$release_endpoint" application/vnd.github+json "$temporary_dir/release.json"; then
        fail "Cannot fetch the release. Check that a release is published and GH_TOKEN/GITHUB_TOKEN or gh auth login has Contents read access for private repositories."
    fi

    # Resolve both assets from one release response so a new release appearing
    # during installation cannot mix a binary and checksum from different tags.
    python3 - "$temporary_dir" "$asset" "$repository" "$release_version" <<'PY'
import json
from pathlib import Path
import re
import sys

folder, asset, repository, requested_version = sys.argv[1:]
folder = Path(folder)
try:
    release = json.loads((folder / "release.json").read_text())
    version = release.get("tag_name", "")
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("release must have a stable vMAJOR.MINOR.PATCH tag")
    if release.get("draft") or release.get("prerelease"):
        raise ValueError("release must be published and stable")
    if requested_version != "latest" and version != requested_version:
        raise ValueError(f"requested {requested_version}, received {version}")
    assets = {entry["name"]: entry for entry in release["assets"]}
    for name, output in ((asset, "binary-url"), (asset + ".sha256", "checksum-url")):
        entry = assets.get(name)
        if entry is None:
            raise ValueError(f"release {version} is missing asset {name}")
        identifier = entry["id"]
        if type(identifier) is not int or identifier <= 0:
            raise ValueError(f"invalid asset ID for {name}")
        (folder / output).write_text(f"https://api.github.com/repos/{repository}/releases/assets/{identifier}")
    (folder / "version").write_text(version)
except (ValueError, KeyError, TypeError, OSError) as error:
    sys.exit(f"mf installer: {error}")
PY

    download "$(cat "$temporary_dir/binary-url")" application/octet-stream "$temporary_dir/mf" || fail "Failed to download $asset."
    download "$(cat "$temporary_dir/checksum-url")" application/octet-stream "$temporary_dir/checksum" || fail "Failed to download the checksum for $asset."
    read -r expected_hash checksum_filename < "$temporary_dir/checksum" || fail "Invalid checksum file."
    case "$expected_hash" in
        ''|*[!0-9a-fA-F]*) fail "Invalid SHA-256 checksum." ;;
    esac
    [ "${#expected_hash}" -eq 64 ] || fail "Invalid SHA-256 checksum length."
    [ "$checksum_filename" = "$asset" ] || fail "Checksum names the wrong executable."
    if [ "$hash_command" = sha256sum ]; then
        actual_hash=$(sha256sum "$temporary_dir/mf")
    else
        actual_hash=$(shasum -a 256 "$temporary_dir/mf")
    fi
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || fail "SHA-256 verification failed; the existing installation has not been changed."

    mkdir -p "$install_dir"
    [ ! -d "$install_dir/mf" ] || fail "$install_dir/mf is a directory."
    # Stage beside the destination so replacing an existing binary is atomic.
    staged_file=$(mktemp "$install_dir/.mf.XXXXXX")
    cp "$temporary_dir/mf" "$staged_file"
    chmod 755 "$staged_file"
    mv -f "$staged_file" "$install_dir/mf"
    staged_file=''
    printf 'Installed mf %s to %s/mf\n' "$(cat "$temporary_dir/version")" "$install_dir"
    case ":${PATH:-}:" in
        *":$install_dir:"*) ;;
        *) printf 'Add %s to your PATH to run mf from any directory.\n' "$install_dir" ;;
    esac
}

fail() {
    printf 'mf installer: %s\n' "$*" >&2
    exit 1
}

cleanup() {
    if [ -n "$staged_file" ]; then rm -f "$staged_file"; fi
    if [ -n "$temporary_dir" ]; then rm -rf "$temporary_dir"; fi
}

download() {
    if [ -n "${GH_TOKEN:-}" ]; then
        curl --fail --silent --show-error --location --retry 3 \
            --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
            -H "Authorization: Bearer $GH_TOKEN" -H "Accept: $2" -o "$3" "$1"
    else
        curl --fail --silent --show-error --location --retry 3 \
            --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
            -H "Accept: $2" -o "$3" "$1"
    fi
}

main "$@"
