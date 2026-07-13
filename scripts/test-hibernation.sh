#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
prefix="preview-hibernation-e2e-$$"
dind_container="$prefix-docker"
network="$prefix"
volume="$prefix-payloads"
orchestrator_image="$prefix:local"
orchestrator_container="$prefix-orchestrator"
traefik_container="$prefix-traefik"
legacy_container="$prefix-legacy"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/preview-hibernation-e2e.XXXXXX")
deployment_id=""
preview_container=""
inner_docker_host=""
dashboard_token="0123456789abcdef0123456789abcdef"
dashboard_origin="http://api.localhost"

inner_docker() {
	env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
		docker --host "$inner_docker_host" "$@"
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	set +e
	if [ -n "$inner_docker_host" ] && [ "$status" -ne 0 ]; then
		inner_docker logs "$orchestrator_container" >&2
		inner_docker logs "$traefik_container" >&2
	fi
	if [ -n "$deployment_id" ] && [ -n "${orchestrator_port:-}" ]; then
		curl -sS --connect-timeout 1 --max-time 5 -X DELETE "http://127.0.0.1:$orchestrator_port/v1/deployments/$deployment_id" >/dev/null
	fi
	if [ -n "$inner_docker_host" ]; then
		if [ -n "$preview_container" ]; then
			inner_docker rm -f "$preview_container" >/dev/null 2>&1
		fi
		inner_docker rm -f "$orchestrator_container" "$traefik_container" "$legacy_container" >/dev/null 2>&1
		if [ -n "$deployment_id" ]; then
			inner_docker image rm "preview-deployment/$deployment_id:latest" >/dev/null 2>&1
		fi
		inner_docker image rm "$orchestrator_image" >/dev/null 2>&1
		inner_docker volume rm "$volume" >/dev/null 2>&1
		inner_docker network rm "$network" >/dev/null 2>&1
	fi
	docker rm -f "$dind_container" >/dev/null 2>&1
	rm -rf "$temporary"
	exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
	printf 'hibernation integration test: %s\n' "$1" >&2
	exit 1
}

command -v docker >/dev/null 2>&1 || fail "docker is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v go >/dev/null 2>&1 || fail "Go is required"

cd "$root"
docker info >/dev/null
docker run -d \
	--privileged \
	--name "$dind_container" \
	-p 127.0.0.1::2375 \
	-p 127.0.0.1::18080 \
	-p 127.0.0.1::18081 \
	-e DOCKER_TLS_CERTDIR= \
	docker:27-dind \
	--host=tcp://0.0.0.0:2375 \
	--host=unix:///var/run/docker.sock >/dev/null
dind_port=$(docker port "$dind_container" 2375/tcp | awk -F: 'NR == 1 { print $NF }')
traefik_port=$(docker port "$dind_container" 18080/tcp | awk -F: 'NR == 1 { print $NF }')
orchestrator_port=$(docker port "$dind_container" 18081/tcp | awk -F: 'NR == 1 { print $NF }')
[ -n "$dind_port" ] && [ -n "$traefik_port" ] && [ -n "$orchestrator_port" ] || fail "could not resolve disposable Docker ports"
inner_docker_host="tcp://127.0.0.1:$dind_port"

attempt=0
until inner_docker info >/dev/null 2>&1; do
	attempt=$((attempt + 1))
	[ "$attempt" -lt 80 ] || fail "disposable Docker daemon did not become ready"
	sleep 0.25
done

inner_docker network create "$network" >/dev/null
inner_docker volume create "$volume" >/dev/null
inner_docker run --rm -v "$volume:/payloads" alpine:3.21 chmod 0700 /payloads
inner_docker build -t "$orchestrator_image" .

inner_docker run -d \
	--name "$traefik_container" \
	--network "$network" \
	-p 18080:80 \
	-v /var/run/docker.sock:/var/run/docker.sock:ro \
	traefik:v3.6.23 \
	--providers.docker=true \
	--providers.docker.endpoint=unix:///var/run/docker.sock \
	--providers.docker.exposedbydefault=false \
	--providers.docker.network="$network" \
	--entrypoints.web.address=:80 \
	--log.level=INFO >/dev/null

inner_docker run -d \
	--name "$orchestrator_container" \
	--network "$network" \
	--network-alias orchestrator \
	-p 18081:8080 \
	-v /var/run/docker.sock:/var/run/docker.sock:ro \
	-v "$volume:/payloads" \
	--label traefik.enable=true \
	--label "traefik.docker.network=$network" \
	--label 'traefik.http.routers.dashboard.rule=Host(`api.localhost`) && (Path(`/`) || (Method(`POST`) && Path(`/dashboard/hibernate`)))' \
	--label traefik.http.routers.dashboard.entrypoints=web \
	--label traefik.http.routers.dashboard.service=dashboard \
	--label traefik.http.services.dashboard.loadbalancer.server.port=8080 \
	-e LISTEN_ADDR=:8080 \
	-e DOCKER_SOCKET=/var/run/docker.sock \
	-e DOCKER_NETWORK="$network" \
	-e PREVIEW_DOMAIN=localhost \
	-e API_HOST=api.localhost \
	-e PUBLIC_SCHEME=http \
	-e PAYLOAD_DIR=/payloads \
	-e RUNTIME_IMAGE=alpine:3.21 \
	-e TRAEFIK_ENTRYPOINT=web \
	-e PREVIEW_IDLE_TIMEOUT=4s \
	-e PREVIEW_IDLE_CHECK_INTERVAL=500ms \
	-e DASHBOARD_TOKEN="$dashboard_token" \
	-e DASHBOARD_ORIGIN="$dashboard_origin" \
	"$orchestrator_image" >/dev/null

# This simulates a preview created before hibernation labels existed. It must
# remain running even though the new orchestrator has hibernation enabled.
inner_docker run -d \
	--name "$legacy_container" \
	--network "$network" \
	--label com.preview-deployment.managed=true \
	--label com.preview-deployment.id=bbbbbbbbbbbb \
	alpine:3.21 sleep 300 >/dev/null

attempt=0
until curl -fsS --connect-timeout 1 --max-time 3 "http://127.0.0.1:$orchestrator_port/healthz" >/dev/null; do
	attempt=$((attempt + 1))
	[ "$attempt" -lt 40 ] || fail "orchestrator did not become healthy"
	sleep 0.25
done

attempt=0
while :; do
	dashboard_status=$(curl -sS --connect-timeout 1 --max-time 3 -o /dev/null -w '%{http_code}' \
		-H 'Host: api.localhost' "http://127.0.0.1:$traefik_port/" || true)
	[ "$dashboard_status" = "401" ] && break
	attempt=$((attempt + 1))
	[ "$attempt" -lt 40 ] || fail "authenticated dashboard route did not become ready"
	sleep 0.25
done

public_api_status=$(curl -sS --connect-timeout 1 --max-time 3 -o /dev/null -w '%{http_code}' \
	-H 'Host: api.localhost' "http://127.0.0.1:$traefik_port/v1/deployments")
[ "$public_api_status" = "404" ] || fail "public Traefik route exposed /v1 with HTTP $public_api_status"

if inner_docker run --rm --network "$network" alpine:3.21 \
	sh -c "wget -q -T 2 -O - http://$traefik_container:8080/api/rawdata >/dev/null 2>&1"; then
	fail "Traefik diagnostic API is reachable from a preview network peer"
fi

case "$(inner_docker info --format '{{.Architecture}}')" in
	x86_64 | amd64) goarch=amd64 ;;
	aarch64 | arm64) goarch=arm64 ;;
	*) fail "unsupported Docker architecture" ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$temporary/hello" ./examples/hello
deployment_json=$(GOCACHE="${GOCACHE:-$temporary/go-cache}" PREVIEWCTL_API_URL="http://127.0.0.1:$orchestrator_port" \
	go run ./cmd/previewctl deploy --manifest examples/hello/preview.json --output json "$temporary/hello")
preview_container=$(inner_docker ps -aq --no-trunc \
	--filter "network=$network" \
	--filter "label=com.preview-deployment.hibernation=v1")
[ -n "$preview_container" ] || fail "deployed preview container was not found"
deployment_id=$(inner_docker inspect --format '{{index .Config.Labels "com.preview-deployment.id"}}' "$preview_container")
[ "${#deployment_id}" -eq 12 ] || fail "deployed preview did not contain a valid deployment ID"
printf '%s\n' "$deployment_json" | grep -q "\"$deployment_id\"" || fail "previewctl output did not contain the deployed preview ID"
preview_host="$deployment_id.localhost"

attempt=0
while :; do
	if body=$(curl -sS --connect-timeout 1 --max-time 3 -H "Host: $preview_host" "http://127.0.0.1:$traefik_port/"); then
		case "$body" in
			*"hello from a preview deployment"*) break ;;
		esac
	fi
	attempt=$((attempt + 1))
	[ "$attempt" -lt 40 ] || fail "preview did not become reachable through Traefik"
	sleep 0.25
done

# Requests arriving inside the idle window must keep the same container alive.
attempt=0
while [ "$attempt" -lt 7 ]; do
	curl -fsS --connect-timeout 1 --max-time 3 -H "Host: $preview_host" "http://127.0.0.1:$traefik_port/" >/dev/null
	state=$(inner_docker inspect --format '{{.State.Status}}' "$preview_container")
	[ "$state" = "running" ] || fail "preview stopped while requests were arriving"
	attempt=$((attempt + 1))
	sleep 1
done
[ "$(inner_docker inspect --format '{{.State.Status}}' "$legacy_container")" = "running" ] || fail "legacy preview without wake labels was hibernated"

dashboard_html="$temporary/dashboard.html"
curl -fsS --connect-timeout 1 --max-time 5 \
	-u "preview:$dashboard_token" \
	-H 'Host: api.localhost' \
	"http://127.0.0.1:$traefik_port/" >"$dashboard_html"
grep -q '>Active<' "$dashboard_html" || fail "dashboard did not report the running preview as active"
grep -q 'Hibernate now' "$dashboard_html" || fail "dashboard did not render the manual hibernation button"
if grep -Fq "$dashboard_token" "$dashboard_html"; then
	fail "dashboard leaked its Basic Auth token"
fi
wake_token=$(inner_docker inspect --format '{{index .Config.Labels "com.preview-deployment.wake-token"}}' "$preview_container")
[ "${#wake_token}" -eq 32 ] || fail "preview did not have a valid wake token"
if grep -Fq "$wake_token" "$dashboard_html"; then
	fail "dashboard leaked the preview wake token"
fi
csrf_token=$(tr '>' '\n' <"$dashboard_html" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -n 1)
[ "${#csrf_token}" -eq 64 ] || fail "dashboard did not render a valid CSRF token"

manual_headers="$temporary/manual.headers"
manual_status=$(curl -sS --connect-timeout 1 --max-time 10 \
	-u "preview:$dashboard_token" \
	-H 'Host: api.localhost' \
	-H "Origin: $dashboard_origin" \
	-H 'Content-Type: application/x-www-form-urlencoded' \
	--data "id=$deployment_id&csrf=$csrf_token" \
	-D "$manual_headers" -o /dev/null -w '%{http_code}' \
	"http://127.0.0.1:$traefik_port/dashboard/hibernate")
[ "$manual_status" = "303" ] || fail "manual dashboard hibernation returned HTTP $manual_status"
grep -qi '^Location: /' "$manual_headers" || fail "manual dashboard hibernation did not redirect to the dashboard"

attempt=0
while :; do
	state=$(inner_docker inspect --format '{{.State.Status}}' "$preview_container")
	[ "$state" = "exited" ] && break
	[ "$state" = "running" ] || fail "preview entered unexpected state $state while hibernating"
	attempt=$((attempt + 1))
	[ "$attempt" -lt 40 ] || fail "preview did not hibernate after the dashboard action"
	sleep 0.25
done

curl -fsS --connect-timeout 1 --max-time 5 \
	-u "preview:$dashboard_token" \
	-H 'Host: api.localhost' \
	"http://127.0.0.1:$traefik_port/" >"$dashboard_html"
grep -q '>Hibernated<' "$dashboard_html" || fail "dashboard did not report the manually stopped preview as hibernated"
if grep -q 'Hibernate now' "$dashboard_html"; then
	fail "dashboard left the hibernation button enabled for a stopped preview"
fi

headers="$temporary/resume.headers"
body_file="$temporary/resume.html"
status=$(curl -sS --connect-timeout 1 --max-time 5 -D "$headers" -o "$body_file" -w '%{http_code}' \
	-H "Host: $preview_host" "http://127.0.0.1:$traefik_port/")
[ "$status" = "503" ] || fail "cold request returned HTTP $status instead of the resume splash"
grep -q "Resuming this preview" "$body_file" || fail "resume splash did not contain its status message"
grep -qi '^Retry-After:' "$headers" || fail "resume splash did not include Retry-After"
grep -qi '^Cache-Control:.*no-store' "$headers" || fail "resume splash was cacheable"

attempt=0
while :; do
	if body=$(curl -sS --connect-timeout 1 --max-time 3 -H "Host: $preview_host" "http://127.0.0.1:$traefik_port/"); then
		case "$body" in
			*"hello from a preview deployment"*) break ;;
		esac
	fi
	attempt=$((attempt + 1))
	[ "$attempt" -lt 60 ] || fail "preview did not finish resuming"
	sleep 0.25
done
[ "$(inner_docker inspect --format '{{.State.Status}}' "$preview_container")" = "running" ] || fail "resumed container is not running"
[ "$(inner_docker inspect --format '{{.Id}}' "$preview_container")" = "$preview_container" ] || fail "resume replaced the container instead of restarting it"

# A resumed preview should be eligible for hibernation again after its new
# last-request deadline.
attempt=0
while :; do
	state=$(inner_docker inspect --format '{{.State.Status}}' "$preview_container")
	[ "$state" = "exited" ] && break
	[ "$state" = "running" ] || fail "resumed preview entered unexpected state $state while hibernating"
	attempt=$((attempt + 1))
	[ "$attempt" -lt 40 ] || fail "resumed preview did not hibernate a second time"
	sleep 0.25
done

printf 'hibernation integration test passed (deployment %s)\n' "$deployment_id"
