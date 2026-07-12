#!/bin/sh

set -eu

PROGRAM=previewctl-installer
REPOSITORY=${PREVIEW_DEPLOYMENT_REPOSITORY:-imeredith/preview-deployment}
VERSION=${PREVIEWCTL_VERSION:-latest}

if [ -n "${PREVIEWCTL_INSTALL_DIR:-}" ]; then
    INSTALL_DIR=$PREVIEWCTL_INSTALL_DIR
else
    if [ -z "${HOME:-}" ]; then
        printf '%s: HOME is not set; use --install-dir or PREVIEWCTL_INSTALL_DIR\n' "$PROGRAM" >&2
        exit 1
    fi
    INSTALL_DIR=$HOME/.local/bin
fi

TMP_DIR=
DESTINATION_TMP=

say() {
    printf '%s\n' "$*"
}

die() {
    printf '%s: %s\n' "$PROGRAM" "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Install or update previewctl from a GitHub release.

Usage:
  install-cli.sh [--version TAG] [--install-dir DIR] [--repository OWNER/REPO]

Options:
  --version TAG          Exact release tag, or "latest" (default).
  --install-dir DIR      Destination directory (default: ~/.local/bin).
  --repository REPO      GitHub repository (default: imeredith/preview-deployment).
  -h, --help             Show this help.

Environment:
  PREVIEWCTL_VERSION             Same as --version.
  PREVIEWCTL_INSTALL_DIR         Same as --install-dir.
  PREVIEW_DEPLOYMENT_REPOSITORY  Same as --repository.
EOF
}

cleanup() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
    if [ -n "$DESTINATION_TMP" ] && [ -e "$DESTINATION_TMP" ]; then
        rm -f "$DESTINATION_TMP"
    fi
    if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
    exit "$cleanup_status"
}

trap cleanup 0
trap 'exit 1' HUP INT TERM

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -ge 2 ] || die "--version requires a value"
            VERSION=$2
            shift 2
            ;;
        --version=*)
            VERSION=${1#*=}
            shift
            ;;
        --install-dir)
            [ "$#" -ge 2 ] || die "--install-dir requires a value"
            INSTALL_DIR=$2
            shift 2
            ;;
        --install-dir=*)
            INSTALL_DIR=${1#*=}
            shift
            ;;
        --repository)
            [ "$#" -ge 2 ] || die "--repository requires a value"
            REPOSITORY=$2
            shift 2
            ;;
        --repository=*)
            REPOSITORY=${1#*=}
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            die "unknown argument: $1"
            ;;
    esac
done

[ -n "$INSTALL_DIR" ] || die "install directory must not be empty"

case "$REPOSITORY" in
    */*)
        repository_owner=${REPOSITORY%%/*}
        repository_name=${REPOSITORY#*/}
        ;;
    *)
        die "repository must be in OWNER/REPO form"
        ;;
esac
case "$repository_owner" in
    ''|*[!A-Za-z0-9_.-]*) die "repository owner contains unsupported characters" ;;
esac
case "$repository_name" in
    ''|*/*|*[!A-Za-z0-9_.-]*) die "repository name contains unsupported characters" ;;
esac

for required_command in curl uname mktemp tar awk mkdir cp chmod mv tr; do
    command -v "$required_command" >/dev/null 2>&1 || die "required command not found: $required_command"
done

case "$(uname -s)" in
    Linux) TARGET_OS=linux ;;
    Darwin) TARGET_OS=darwin ;;
    *) die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
    x86_64|amd64) TARGET_ARCH=amd64 ;;
    arm64|aarch64) TARGET_ARCH=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

fetch() {
    fetch_url=$1
    fetch_destination=$2
    curl \
        --fail \
        --silent \
        --show-error \
        --location \
        --proto '=https' \
        --tlsv1.2 \
        --retry 3 \
        --retry-delay 1 \
        --connect-timeout 10 \
        --max-time 300 \
        --output "$fetch_destination" \
        "$fetch_url"
}

resolve_latest_tag() {
    latest_url=https://github.com/$REPOSITORY/releases/latest
    effective_url=$(curl \
        --fail \
        --silent \
        --show-error \
        --location \
        --proto '=https' \
        --tlsv1.2 \
        --retry 3 \
        --retry-delay 1 \
        --connect-timeout 10 \
        --max-time 120 \
        --output /dev/null \
        --write-out '%{url_effective}' \
        "$latest_url")
    resolved_tag=${effective_url##*/}
    [ -n "$resolved_tag" ] && [ "$resolved_tag" != latest ] || die "could not resolve the latest release tag"
    printf '%s\n' "$resolved_tag"
}

if [ "$VERSION" = latest ]; then
    VERSION=$(resolve_latest_tag)
fi
case "$VERSION" in
    ''|*[!A-Za-z0-9._-]*) die "release tag contains unsupported characters: $VERSION" ;;
esac

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/previewctl-install.XXXXXX") || die "could not create temporary directory"
CHECKSUMS=$TMP_DIR/checksums.txt
ASSET=previewctl_${VERSION}_${TARGET_OS}_${TARGET_ARCH}.tar.gz
ARCHIVE=$TMP_DIR/$ASSET
RELEASE_BASE=https://github.com/$REPOSITORY/releases/download/$VERSION

say "Downloading previewctl $VERSION for $TARGET_OS/$TARGET_ARCH..."
fetch "$RELEASE_BASE/checksums.txt" "$CHECKSUMS"
fetch "$RELEASE_BASE/$ASSET" "$ARCHIVE"

expected_checksum=$(awk -v asset="$ASSET" '$2 == asset || $2 == "*" asset { print $1; exit }' "$CHECKSUMS")
case "$expected_checksum" in
    ''|*[!A-Fa-f0-9]*) die "checksums.txt has no valid SHA-256 entry for $ASSET" ;;
esac
[ "${#expected_checksum}" -eq 64 ] || die "checksums.txt has an invalid SHA-256 entry for $ASSET"

if command -v sha256sum >/dev/null 2>&1; then
    actual_checksum=$(sha256sum "$ARCHIVE" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
    actual_checksum=$(shasum -a 256 "$ARCHIVE" | awk '{ print $1 }')
else
    die "sha256sum or shasum is required to verify the download"
fi

expected_checksum=$(printf '%s' "$expected_checksum" | tr 'A-F' 'a-f')
actual_checksum=$(printf '%s' "$actual_checksum" | tr 'A-F' 'a-f')
[ "$actual_checksum" = "$expected_checksum" ] || die "SHA-256 verification failed for $ASSET"

archive_entries=$(tar -tzf "$ARCHIVE") || die "could not inspect $ASSET"
case "$archive_entries" in
    previewctl|./previewctl) ;;
    *) die "$ASSET must contain only the previewctl binary" ;;
esac

EXTRACT_DIR=$TMP_DIR/extract
mkdir -p "$EXTRACT_DIR"
tar -xzf "$ARCHIVE" -C "$EXTRACT_DIR"
if [ -f "$EXTRACT_DIR/previewctl" ]; then
    EXTRACTED_BINARY=$EXTRACT_DIR/previewctl
else
    EXTRACTED_BINARY=$EXTRACT_DIR/./previewctl
fi
[ -f "$EXTRACTED_BINARY" ] || die "$ASSET did not contain previewctl"

mkdir -p "$INSTALL_DIR"
DESTINATION_TMP=$(mktemp "$INSTALL_DIR/.previewctl.XXXXXX") || die "cannot write to $INSTALL_DIR"
cp "$EXTRACTED_BINARY" "$DESTINATION_TMP"
chmod 0755 "$DESTINATION_TMP"
mv -f "$DESTINATION_TMP" "$INSTALL_DIR/previewctl"
DESTINATION_TMP=

say "Installed previewctl $VERSION to $INSTALL_DIR/previewctl"
case ":${PATH:-}:" in
    *:"$INSTALL_DIR":*) ;;
    *) say "Add $INSTALL_DIR to PATH to run previewctl directly." ;;
esac
