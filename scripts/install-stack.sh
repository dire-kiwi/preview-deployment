#!/bin/sh

set -eu

PROGRAM=preview-deployment-installer
REPOSITORY=${PREVIEW_DEPLOYMENT_REPOSITORY:-dire-kiwi/preview-deployment}
REQUESTED_VERSION=${PREVIEW_DEPLOYMENT_VERSION:-latest}
CLI_VERSION=${PREVIEWCTL_VERSION:-latest}

if [ -n "${PREVIEW_DEPLOYMENT_INSTALL_DIR:-}" ]; then
    INSTALL_DIR=$PREVIEW_DEPLOYMENT_INSTALL_DIR
else
    if [ -z "${HOME:-}" ]; then
        printf '%s: HOME is not set; use --install-dir or PREVIEW_DEPLOYMENT_INSTALL_DIR\n' "$PROGRAM" >&2
        exit 1
    fi
    INSTALL_DIR=${XDG_DATA_HOME:-$HOME/.local/share}/preview-deployment
fi

if [ -n "${PREVIEW_DEPLOYMENT_ENV_FILE:-}" ]; then
    ENV_FILE=$PREVIEW_DEPLOYMENT_ENV_FILE
    ENV_FILE_EXPLICIT=true
else
    ENV_FILE=$INSTALL_DIR/.env
    ENV_FILE_EXPLICIT=false
fi

if [ -n "${PREVIEWCTL_INSTALL_DIR:-}" ]; then
    CLI_INSTALL_DIR=$PREVIEWCTL_INSTALL_DIR
else
    if [ -z "${HOME:-}" ]; then
        printf '%s: HOME is not set; set PREVIEWCTL_INSTALL_DIR\n' "$PROGRAM" >&2
        exit 1
    fi
    CLI_INSTALL_DIR=$HOME/.local/bin
fi

TMP_DIR=

say() {
    printf '%s\n' "$*"
}

die() {
    printf '%s: %s\n' "$PROGRAM" "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Install previewctl, then use it to start or update the preview deployment stack.

Usage:
  install-stack.sh [--version TAG] [--install-dir DIR] [--env-file FILE]
                   [--repository OWNER/REPO]

Options:
  --version TAG          Exact stack release tag, or "latest" (default).
  --install-dir DIR      Stack directory (default: $XDG_DATA_HOME/preview-deployment
                         or ~/.local/share/preview-deployment).
  --env-file FILE        Stack environment file (default: INSTALL_DIR/.env).
  --repository REPO      GitHub repository (default: dire-kiwi/preview-deployment).
  -h, --help             Show this help.

Environment:
  PREVIEW_DEPLOYMENT_VERSION      Same as --version.
  PREVIEW_DEPLOYMENT_INSTALL_DIR  Same as --install-dir.
  PREVIEW_DEPLOYMENT_ENV_FILE     Same as --env-file.
  PREVIEW_DEPLOYMENT_REPOSITORY   Same as --repository.
  PREVIEWCTL_INSTALL_DIR          previewctl destination (default: ~/.local/bin).
  PREVIEWCTL_VERSION              Lifecycle-capable CLI release override. A
                                  published installer defaults to its own tag.
  PREVIEW_PAYLOAD_DIR             Fresh-install payload directory override;
                                  exported to previewctl when set.

This compatibility installer contains no Docker Compose rollout logic. It
checksum-verifies its release's install-cli.sh, installs previewctl,
then delegates the complete stack transaction to `previewctl start` for a new
installation or `previewctl update` for an existing installation.
EOF
}

cleanup() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
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
            REQUESTED_VERSION=$2
            shift 2
            ;;
        --version=*)
            REQUESTED_VERSION=${1#*=}
            shift
            ;;
        --install-dir)
            [ "$#" -ge 2 ] || die "--install-dir requires a value"
            INSTALL_DIR=$2
            if [ "$ENV_FILE_EXPLICIT" = false ]; then
                ENV_FILE=$INSTALL_DIR/.env
            fi
            shift 2
            ;;
        --install-dir=*)
            INSTALL_DIR=${1#*=}
            if [ "$ENV_FILE_EXPLICIT" = false ]; then
                ENV_FILE=$INSTALL_DIR/.env
            fi
            shift
            ;;
        --env-file)
            [ "$#" -ge 2 ] || die "--env-file requires a value"
            ENV_FILE=$2
            ENV_FILE_EXPLICIT=true
            shift 2
            ;;
        --env-file=*)
            ENV_FILE=${1#*=}
            ENV_FILE_EXPLICIT=true
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
[ -n "$ENV_FILE" ] || die "environment file must not be empty"
[ -n "$CLI_INSTALL_DIR" ] || die "PREVIEWCTL_INSTALL_DIR must not be empty"

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
case "$REQUESTED_VERSION" in
    ''|*[!A-Za-z0-9._-]*) die "release tag contains unsupported characters: $REQUESTED_VERSION" ;;
esac

for required_command in curl mktemp awk cp chmod mkdir sed tr; do
    command -v "$required_command" >/dev/null 2>&1 || die "required command not found: $required_command"
done

fetch() {
    fetch_url=$1
    fetch_destination=$2
    curl -q \
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
    effective_url=$(curl -q \
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
    case "$resolved_tag" in
        *[!A-Za-z0-9._-]*) die "resolved release tag contains unsupported characters" ;;
    esac
    printf '%s\n' "$resolved_tag"
}

if [ "$REQUESTED_VERSION" = latest ]; then
    RESOLVED_VERSION=$(resolve_latest_tag)
else
    RESOLVED_VERSION=$REQUESTED_VERSION
fi
if [ "$CLI_VERSION" = latest ]; then
    CLI_VERSION=$(resolve_latest_tag)
fi
case "$CLI_VERSION" in
    ''|*[!A-Za-z0-9._-]*) die "CLI release tag contains unsupported characters: $CLI_VERSION" ;;
esac

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/preview-stack-bootstrap.XXXXXX") || die "could not create temporary directory"
chmod 0700 "$TMP_DIR"
CHECKSUMS=$TMP_DIR/checksums.txt
INSTALL_CLI=$TMP_DIR/install-cli.sh
RELEASE_BASE=https://github.com/$REPOSITORY/releases/download/$CLI_VERSION

say "Downloading verified previewctl installer from $CLI_VERSION..."
fetch "$RELEASE_BASE/checksums.txt" "$CHECKSUMS"
fetch "$RELEASE_BASE/install-cli.sh" "$INSTALL_CLI"

expected_checksum=$(awk '$2 == "install-cli.sh" || $2 == "*install-cli.sh" { print $1 }' "$CHECKSUMS")
case "$expected_checksum" in
    ''|*[!A-Fa-f0-9]*) die "checksums.txt has no unique valid SHA-256 entry for install-cli.sh" ;;
esac
[ "${#expected_checksum}" -eq 64 ] || die "checksums.txt has no unique valid SHA-256 entry for install-cli.sh"

if command -v sha256sum >/dev/null 2>&1; then
    actual_checksum=$(sha256sum "$INSTALL_CLI" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
    actual_checksum=$(shasum -a 256 "$INSTALL_CLI" | awk '{ print $1 }')
else
    die "sha256sum or shasum is required to verify install-cli.sh"
fi
expected_checksum=$(printf '%s' "$expected_checksum" | tr 'A-F' 'a-f')
actual_checksum=$(printf '%s' "$actual_checksum" | tr 'A-F' 'a-f')
[ "$actual_checksum" = "$expected_checksum" ] || die "SHA-256 verification failed for install-cli.sh"
chmod 0500 "$INSTALL_CLI"

say "Installing previewctl $CLI_VERSION..."
sh "$INSTALL_CLI" \
    --version "$CLI_VERSION" \
    --install-dir "$CLI_INSTALL_DIR" \
    --repository "$REPOSITORY"

PREVIEWCTL=$CLI_INSTALL_DIR/previewctl
[ -f "$PREVIEWCTL" ] && [ -x "$PREVIEWCTL" ] || die "previewctl was not installed at $PREVIEWCTL"

if [ ! -e "$INSTALL_DIR/VERSION" ] && [ ! -e "$INSTALL_DIR/compose.yaml" ]; then
    stack_operation=start
elif [ -f "$INSTALL_DIR/VERSION" ] && [ -f "$INSTALL_DIR/compose.yaml" ]; then
    installed_version=$(sed -n '1p' "$INSTALL_DIR/VERSION")
    if [ "$installed_version" = "$RESOLVED_VERSION" ]; then
        stack_operation=start
    else
        stack_operation=update
    fi
else
    die "stack installation is partial; use previewctl status before recovery"
fi

say "Delegating stack $stack_operation to previewctl..."
if [ -n "${PREVIEW_PAYLOAD_DIR:-}" ]; then
    # Make the documented fresh-install override explicit even when a caller
    # invokes this script from a shell where it was not previously exported.
    export PREVIEW_PAYLOAD_DIR
fi
set -- \
    "$PREVIEWCTL" "$stack_operation" \
    --install-dir "$INSTALL_DIR" \
    --repository "$REPOSITORY" \
    --version "$RESOLVED_VERSION"
if [ "$ENV_FILE_EXPLICIT" = true ]; then
    set -- "$@" --env-file "$ENV_FILE"
fi
"$@"
