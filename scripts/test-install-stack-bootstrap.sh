#!/bin/sh

set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/preview-stack-bootstrap-test.XXXXXX")
BOOTSTRAP=$ROOT/scripts/install-stack.sh

cleanup() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
    rm -rf "$TEST_ROOT"
    exit "$cleanup_status"
}

trap cleanup 0
trap 'exit 1' HUP INT TERM

FIXTURE_RELEASE=$TEST_ROOT/release
FAKE_BIN=$TEST_ROOT/bin
mkdir -p "$FIXTURE_RELEASE" "$FAKE_BIN"

cat > "$FIXTURE_RELEASE/install-cli.sh" <<'INSTALLER'
#!/bin/sh
set -eu

printf '%s\n' "$*" >> "$FIXTURE_INSTALL_LOG"
destination=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --install-dir)
            destination=$2
            shift 2
            ;;
        *)
            shift
            ;;
    esac
done
[ -n "$destination" ]
mkdir -p "$destination"
cp "$FIXTURE_PREVIEWCTL" "$destination/previewctl"
chmod 0755 "$destination/previewctl"
INSTALLER
chmod 0755 "$FIXTURE_RELEASE/install-cli.sh"

cat > "$TEST_ROOT/previewctl" <<'PREVIEWCTL'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FIXTURE_PREVIEWCTL_LOG"
PREVIEWCTL
chmod 0755 "$TEST_ROOT/previewctl"

cat > "$FAKE_BIN/curl" <<'CURL'
#!/bin/sh
set -eu

destination=
write_effective=false
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output)
            destination=$2
            shift 2
            ;;
        --write-out)
            write_effective=true
            shift 2
            ;;
        --retry|--retry-delay|--connect-timeout|--max-time|--proto)
            shift 2
            ;;
        -q|--fail|--silent|--show-error|--location|--tlsv1.2)
            shift
            ;;
        *)
            url=$1
            shift
            ;;
    esac
done

if [ "$write_effective" = true ]; then
    printf 'https://github.com/%s/releases/tag/v9.9.9' "$FIXTURE_REPOSITORY"
    exit 0
fi

case "$url" in
    */checksums.txt) source=$FIXTURE_RELEASE/checksums.txt ;;
    */install-cli.sh) source=$FIXTURE_RELEASE/install-cli.sh ;;
    *) printf 'unexpected fixture URL: %s\n' "$url" >&2; exit 1 ;;
esac
cp "$source" "$destination"
CURL
chmod 0755 "$FAKE_BIN/curl"

if command -v sha256sum >/dev/null 2>&1; then
    installer_sum=$(sha256sum "$FIXTURE_RELEASE/install-cli.sh" | awk '{ print $1 }')
else
    installer_sum=$(shasum -a 256 "$FIXTURE_RELEASE/install-cli.sh" | awk '{ print $1 }')
fi
printf '%s  install-cli.sh\n' "$installer_sum" > "$FIXTURE_RELEASE/checksums.txt"

run_bootstrap() {
    fixture_repository=$1
    fixture_install_log=$2
    fixture_previewctl_log=$3
    fixture_cli_install_dir=$4
    shift 4
    PATH=$FAKE_BIN:$PATH \
    FIXTURE_RELEASE=$FIXTURE_RELEASE \
    FIXTURE_REPOSITORY=$fixture_repository \
    FIXTURE_INSTALL_LOG=$fixture_install_log \
    FIXTURE_PREVIEWCTL=$TEST_ROOT/previewctl \
    FIXTURE_PREVIEWCTL_LOG=$fixture_previewctl_log \
    PREVIEWCTL_INSTALL_DIR=$fixture_cli_install_dir \
        sh "$BOOTSTRAP" "$@"
}

start_root=$TEST_ROOT/start
mkdir -p "$start_root"
run_bootstrap \
    dire-kiwi/preview-deployment \
    "$start_root/install.log" \
    "$start_root/previewctl.log" \
    "$start_root/bin" \
    --install-dir "$start_root/stack"

grep -Fx -- "--version v9.9.9 --install-dir $start_root/bin --repository dire-kiwi/preview-deployment" "$start_root/install.log" >/dev/null
grep -Fx -- "start --install-dir $start_root/stack --repository dire-kiwi/preview-deployment --version v9.9.9" "$start_root/previewctl.log" >/dev/null

update_root=$TEST_ROOT/update
mkdir -p "$update_root/stack"
: > "$update_root/stack/compose.yaml"
printf 'v1.0.0\n' > "$update_root/stack/VERSION"
run_bootstrap \
    example/preview \
    "$update_root/install.log" \
    "$update_root/previewctl.log" \
    "$update_root/bin" \
    --version v1.2.3 \
    --install-dir "$update_root/stack" \
    --env-file "$update_root/custom.env" \
    --repository example/preview

grep -Fx -- "--version v9.9.9 --install-dir $update_root/bin --repository example/preview" "$update_root/install.log" >/dev/null
grep -Fx -- "update --install-dir $update_root/stack --repository example/preview --version v1.2.3 --env-file $update_root/custom.env" "$update_root/previewctl.log" >/dev/null

same_root=$TEST_ROOT/same
mkdir -p "$same_root/stack"
: > "$same_root/stack/compose.yaml"
printf 'v1.2.3\n' > "$same_root/stack/VERSION"
run_bootstrap \
    example/preview \
    "$same_root/install.log" \
    "$same_root/previewctl.log" \
    "$same_root/bin" \
    --version v1.2.3 \
    --install-dir "$same_root/stack" \
    --repository example/preview

grep -Fx -- "--version v9.9.9 --install-dir $same_root/bin --repository example/preview" "$same_root/install.log" >/dev/null
grep -Fx -- "start --install-dir $same_root/stack --repository example/preview --version v1.2.3" "$same_root/previewctl.log" >/dev/null

# The release workflow embeds only this assignment. Prove that a published
# installer remains pinned to its own CLI tag even when GitHub's latest release
# has moved on to another version.
published_installer=$TEST_ROOT/install-stack-published.sh
sed 's|^CLI_VERSION=.*$|CLI_VERSION=${PREVIEWCTL_VERSION:-v8.8.8}|' \
    "$ROOT/scripts/install-stack.sh" > "$published_installer"
chmod 0755 "$published_installer"
BOOTSTRAP=$published_installer
published_root=$TEST_ROOT/published
mkdir -p "$published_root"
run_bootstrap \
    dire-kiwi/preview-deployment \
    "$published_root/install.log" \
    "$published_root/previewctl.log" \
    "$published_root/bin" \
    --install-dir "$published_root/stack"

grep -Fx -- "--version v8.8.8 --install-dir $published_root/bin --repository dire-kiwi/preview-deployment" "$published_root/install.log" >/dev/null
grep -Fx -- "start --install-dir $published_root/stack --repository dire-kiwi/preview-deployment --version v9.9.9" "$published_root/previewctl.log" >/dev/null

BOOTSTRAP=$ROOT/scripts/install-stack.sh

printf '%064d  install-cli.sh\n' 0 > "$FIXTURE_RELEASE/checksums.txt"
mismatch_root=$TEST_ROOT/mismatch
mkdir -p "$mismatch_root"
if run_bootstrap \
    dire-kiwi/preview-deployment \
    "$mismatch_root/install.log" \
    "$mismatch_root/previewctl.log" \
    "$mismatch_root/bin" \
    --version v1.2.3 \
    --install-dir "$mismatch_root/stack" >/dev/null 2>&1; then
    printf 'bootstrap accepted a mismatched install-cli.sh checksum\n' >&2
    exit 1
fi
[ ! -e "$mismatch_root/install.log" ]
[ ! -e "$mismatch_root/previewctl.log" ]

printf 'install-stack bootstrap tests passed\n'
