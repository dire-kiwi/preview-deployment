#!/bin/sh

set -eu

PROGRAM=preview-deployment-installer
REPOSITORY=${PREVIEW_DEPLOYMENT_REPOSITORY:-dire-kiwi/preview-deployment}
VERSION=${PREVIEW_DEPLOYMENT_VERSION:-latest}

if [ -n "${PREVIEW_DEPLOYMENT_INSTALL_DIR:-}" ]; then
    INSTALL_DIR=$PREVIEW_DEPLOYMENT_INSTALL_DIR
else
    if [ -z "${HOME:-}" ]; then
        printf '%s: HOME is not set; use --install-dir or PREVIEW_DEPLOYMENT_INSTALL_DIR\n' "$PROGRAM" >&2
        exit 1
    fi
    INSTALL_DIR=${XDG_DATA_HOME:-$HOME/.local/share}/preview-deployment
fi
ENV_FILE=${PREVIEW_DEPLOYMENT_ENV_FILE:-$INSTALL_DIR/.env}
if [ -n "${PREVIEW_DEPLOYMENT_ENV_FILE:-}" ]; then
    ENV_FILE_EXPLICIT=true
else
    ENV_FILE_EXPLICIT=false
fi

TMP_DIR=
ATOMIC_TMP=
COMPOSE_TMP=
ENV_EXAMPLE_TMP=
INSTALLED_COMPOSE=$INSTALL_DIR/compose.yaml
INSTALLED_ENV_EXAMPLE=$INSTALL_DIR/.env.example

say() {
    printf '%s\n' "$*"
}

die() {
    printf '%s: %s\n' "$PROGRAM" "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Install or update the preview deployment Docker stack from a GitHub release.

Usage:
  install-stack.sh [--version TAG] [--install-dir DIR] [--env-file FILE]
                   [--repository OWNER/REPO]

Options:
  --version TAG          Exact release/image tag, or "latest" (default).
  --install-dir DIR      Stack directory (default: $XDG_DATA_HOME/preview-deployment
                         or ~/.local/share/preview-deployment).
  --env-file FILE        Compose environment file (default: INSTALL_DIR/.env).
  --repository REPO      GitHub repository (default: dire-kiwi/preview-deployment).
  -h, --help             Show this help.

Environment:
  PREVIEW_DEPLOYMENT_VERSION      Same as --version.
  PREVIEW_DEPLOYMENT_INSTALL_DIR  Same as --install-dir.
  PREVIEW_DEPLOYMENT_ENV_FILE     Same as --env-file.
  PREVIEW_DEPLOYMENT_REPOSITORY   Same as --repository.

An existing .env is never replaced. The selected release tag is exported only
for the pull/up operation and recorded in INSTALL_DIR/VERSION.
EOF
}

cleanup() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
    if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
    if [ -n "$ATOMIC_TMP" ] && [ -e "$ATOMIC_TMP" ]; then
        rm -f "$ATOMIC_TMP"
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
            INSTALLED_COMPOSE=$INSTALL_DIR/compose.yaml
            INSTALLED_ENV_EXAMPLE=$INSTALL_DIR/.env.example
            if [ "$ENV_FILE_EXPLICIT" = false ]; then
                ENV_FILE=$INSTALL_DIR/.env
            fi
            shift 2
            ;;
        --install-dir=*)
            INSTALL_DIR=${1#*=}
            INSTALLED_COMPOSE=$INSTALL_DIR/compose.yaml
            INSTALLED_ENV_EXAMPLE=$INSTALL_DIR/.env.example
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

for required_command in curl uname mktemp awk mkdir dirname cp chmod mv docker tr sleep; do
    command -v "$required_command" >/dev/null 2>&1 || die "required command not found: $required_command"
done

case "$(uname -s)" in
    Linux) HOST_OS=linux ;;
    Darwin) HOST_OS=darwin ;;
    *) die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH=amd64 ;;
    arm64|aarch64) HOST_ARCH=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

docker compose version >/dev/null 2>&1 || die "Docker Compose v2 (docker compose) is required"
docker info >/dev/null 2>&1 || die "cannot connect to the Docker daemon"

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

verify_asset() {
    verify_file=$1
    verify_name=$2
    verify_checksums=$3
    expected_checksum=$(awk -v asset="$verify_name" '$2 == asset || $2 == "*" asset { print $1; exit }' "$verify_checksums")
    case "$expected_checksum" in
        ''|*[!A-Fa-f0-9]*) die "checksums.txt has no valid SHA-256 entry for $verify_name" ;;
    esac
    [ "${#expected_checksum}" -eq 64 ] || die "checksums.txt has an invalid SHA-256 entry for $verify_name"

    if command -v sha256sum >/dev/null 2>&1; then
        actual_checksum=$(sha256sum "$verify_file" | awk '{ print $1 }')
    elif command -v shasum >/dev/null 2>&1; then
        actual_checksum=$(shasum -a 256 "$verify_file" | awk '{ print $1 }')
    else
        die "sha256sum or shasum is required to verify downloads"
    fi
    expected_checksum=$(printf '%s' "$expected_checksum" | tr 'A-F' 'a-f')
    actual_checksum=$(printf '%s' "$actual_checksum" | tr 'A-F' 'a-f')
    [ "$actual_checksum" = "$expected_checksum" ] || die "SHA-256 verification failed for $verify_name"
}

atomic_copy() {
    copy_source=$1
    copy_destination=$2
    copy_mode=$3
    copy_directory=$(dirname "$copy_destination")
    mkdir -p "$copy_directory"
    ATOMIC_TMP=$(mktemp "$copy_directory/.preview-install.XXXXXX") || die "cannot write to $copy_directory"
    cp "$copy_source" "$ATOMIC_TMP"
    chmod "$copy_mode" "$ATOMIC_TMP"
    mv -f "$ATOMIC_TMP" "$copy_destination"
    ATOMIC_TMP=
}

run_compose() {
    compose_file=$1
    shift
    docker compose \
        --project-directory "$INSTALL_DIR" \
        --env-file "$ENV_FILE" \
        --file "$compose_file" \
        "$@"
}

wait_for_health() {
    health_attempt=1
    while [ "$health_attempt" -le 30 ]; do
        if run_compose "$INSTALLED_COMPOSE" exec --no-TTY orchestrator /usr/local/bin/orchestrator healthcheck >/dev/null 2>&1; then
            return 0
        fi
        health_attempt=$((health_attempt + 1))
        sleep 2
    done
    return 1
}

if [ "$VERSION" = latest ]; then
    VERSION=$(resolve_latest_tag)
fi
case "$VERSION" in
    ''|*[!A-Za-z0-9._-]*) die "release tag contains unsupported characters: $VERSION" ;;
esac
export PREVIEW_DEPLOYMENT_VERSION="$VERSION"

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/preview-stack-install.XXXXXX") || die "could not create temporary directory"
CHECKSUMS=$TMP_DIR/checksums.txt
COMPOSE_TMP=$TMP_DIR/compose.yaml
ENV_EXAMPLE_TMP=$TMP_DIR/.env.example
RELEASE_BASE=https://github.com/$REPOSITORY/releases/download/$VERSION

say "Downloading preview deployment $VERSION for $HOST_OS/$HOST_ARCH..."
fetch "$RELEASE_BASE/checksums.txt" "$CHECKSUMS"
fetch "$RELEASE_BASE/compose.yaml" "$COMPOSE_TMP"
fetch "$RELEASE_BASE/env.example" "$ENV_EXAMPLE_TMP"
verify_asset "$COMPOSE_TMP" compose.yaml "$CHECKSUMS"
verify_asset "$ENV_EXAMPLE_TMP" env.example "$CHECKSUMS"

mkdir -p "$INSTALL_DIR"
mkdir -p "$(dirname "$ENV_FILE")"
if [ ! -e "$ENV_FILE" ]; then
    atomic_copy "$ENV_EXAMPLE_TMP" "$ENV_FILE" 0600
    say "Created $ENV_FILE; review it before exposing the service."
elif [ ! -f "$ENV_FILE" ]; then
    die "$ENV_FILE exists but is not a regular file"
else
    say "Preserving existing environment file $ENV_FILE"
fi

run_compose "$COMPOSE_TMP" config --quiet
say "Pulling release images..."
run_compose "$COMPOSE_TMP" pull

PREVIOUS_COMPOSE=$TMP_DIR/previous-compose.yaml
PREVIOUS_VERSION=$TMP_DIR/previous-version
HAD_PREVIOUS_COMPOSE=false
HAD_PREVIOUS_VERSION=false
if [ -f "$INSTALLED_COMPOSE" ]; then
    cp "$INSTALLED_COMPOSE" "$PREVIOUS_COMPOSE"
    HAD_PREVIOUS_COMPOSE=true
fi
if [ -f "$INSTALL_DIR/VERSION" ]; then
    cp "$INSTALL_DIR/VERSION" "$PREVIOUS_VERSION"
    HAD_PREVIOUS_VERSION=true
fi

rollback_stack() {
    if [ "$HAD_PREVIOUS_COMPOSE" != true ]; then
        say "No previous stack metadata was available for automatic rollback."
        return
    fi

    say "Restoring the previous stack configuration..."
    atomic_copy "$PREVIOUS_COMPOSE" "$INSTALLED_COMPOSE" 0644
    if [ "$HAD_PREVIOUS_VERSION" = true ]; then
        rollback_version=$(awk 'NR == 1 { print; exit }' "$PREVIOUS_VERSION")
        case "$rollback_version" in
            ''|*[!A-Za-z0-9._-]*)
                unset PREVIEW_DEPLOYMENT_VERSION
                ;;
            *)
                PREVIEW_DEPLOYMENT_VERSION=$rollback_version
                export PREVIEW_DEPLOYMENT_VERSION
                ;;
        esac
        atomic_copy "$PREVIOUS_VERSION" "$INSTALL_DIR/VERSION" 0644
    else
        unset PREVIEW_DEPLOYMENT_VERSION
    fi

    if ! run_compose "$INSTALLED_COMPOSE" up --detach --remove-orphans; then
        say "Warning: automatic rollback could not reconcile the previous stack." >&2
    fi
}

atomic_copy "$COMPOSE_TMP" "$INSTALLED_COMPOSE" 0644
atomic_copy "$ENV_EXAMPLE_TMP" "$INSTALLED_ENV_EXAMPLE" 0644

say "Reconciling the stack without stopping existing preview containers..."
if ! run_compose "$INSTALLED_COMPOSE" up --detach --remove-orphans; then
    rollback_stack
    die "docker compose up failed"
fi

say "Waiting up to 60 seconds for the orchestrator healthcheck..."
if ! wait_for_health; then
    rollback_stack
    die "the orchestrator did not become healthy within 60 seconds"
fi

VERSION_TMP=$TMP_DIR/VERSION
printf '%s\n' "$VERSION" > "$VERSION_TMP"
atomic_copy "$VERSION_TMP" "$INSTALL_DIR/VERSION" 0644

say "Preview deployment $VERSION is installed in $INSTALL_DIR"
say "Environment: $ENV_FILE"
say "The installer used docker compose pull followed by docker compose up --detach; it did not run down."
