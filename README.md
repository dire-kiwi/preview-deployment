# Preview deployment

A small, self-hosted preview platform built from two long-running containers:

- **Traefik** discovers preview containers from Docker labels and routes each deployment by hostname.
- **Orchestrator** accepts a validated ZIP and owns its build-or-stage, create, start, stop, logs, and delete lifecycle.

There is no database. Docker containers and labels are the source of truth, so previews remain visible across orchestrator upgrades and restarts.

## Install or update the server

Requirements: Docker Engine, the Docker Compose plugin, and `curl`.

```sh
curl --proto '=https' --tlsv1.2 -fsSL \
  https://github.com/dire-kiwi/preview-deployment/releases/latest/download/install-stack.sh | sh
```

The command is idempotent: the first run installs the stack and later runs update it. It downloads a versioned Compose file, verifies its SHA-256 checksum, preserves the existing `.env`, pulls images, and reconciles the running services without taking the shared preview network down.

The default install directory is `${XDG_DATA_HOME:-$HOME/.local/share}/preview-deployment`. Pin a release or choose another directory with installer arguments:

```sh
curl --proto '=https' --tlsv1.2 -fsSL \
  https://github.com/dire-kiwi/preview-deployment/releases/latest/download/install-stack.sh |
  sh -s -- --version v0.1.0 --install-dir /opt/preview-deployment
```

Equivalent environment variables are `PREVIEW_DEPLOYMENT_VERSION`, `PREVIEW_DEPLOYMENT_INSTALL_DIR`, `PREVIEW_DEPLOYMENT_ENV_FILE`, and `PREVIEW_DEPLOYMENT_REPOSITORY`.

Local endpoints use loopback by default:

| Endpoint | Address |
|---|---|
| Orchestrator API | `http://127.0.0.1:8081` or `http://api.localhost` |
| Preview traffic | `http://<deployment-id>.localhost` |

Check the installation with:

```sh
curl --fail http://127.0.0.1:8081/healthz
```

## Install or update `previewctl`

The standalone CLI supports Linux and macOS on amd64 and arm64:

```sh
curl --proto '=https' --tlsv1.2 -fsSL \
  https://github.com/dire-kiwi/preview-deployment/releases/latest/download/install-cli.sh | sh
```

It installs to `~/.local/bin` by default. Ensure that directory is on `PATH`, then inspect or update the installed version:

```sh
previewctl version
previewctl update --check
previewctl update
```

`update --check` is read-only. `update` downloads the matching release artifact, verifies its checksum, and atomically replaces the current executable. The CLI never checks in the background.

The installer also accepts `--version`, `--install-dir`, and `--repository`; equivalent environment variables are `PREVIEWCTL_VERSION`, `PREVIEWCTL_INSTALL_DIR`, and `PREVIEW_DEPLOYMENT_REPOSITORY`.

## Use the CLI

`previewctl` defaults to `http://127.0.0.1:8081`. Set global flags before the command, or use their environment equivalents:

```sh
export PREVIEWCTL_API_URL=https://preview.example.com
export PREVIEWCTL_TOKEN='replace-with-a-secret'

previewctl health
previewctl list
previewctl get 74b52d6cbba9
previewctl logs --tail 100 74b52d6cbba9
previewctl stop 74b52d6cbba9
previewctl start 74b52d6cbba9
previewctl delete 74b52d6cbba9
```

Available global settings are `--api-url` / `PREVIEWCTL_API_URL`, `--token` / `PREVIEWCTL_TOKEN`, and `--timeout` / `PREVIEWCTL_TIMEOUT`. Lifecycle commands support JSON output for automation.

Deploy an existing ZIP:

```sh
previewctl deploy --output json deployment.zip
```

Or give the CLI a Linux executable and optional manifest; it creates the ZIP without requiring a local `zip` command:

```sh
previewctl deploy --manifest preview.json --output json ./app
```

For a custom image, point the same command at a directory containing a
root-level `Dockerfile`. The CLI applies `.dockerignore`, excludes `.git`,
creates one ZIP containing the sanitized build context and manifest, and uploads
that ZIP to the orchestrator. Docker builds and stores the image only on the
preview server:

```sh
previewctl deploy --manifest preview.json --output json .
```

For sources that run in a reusable image already built on the preview host, use
an explicit runtime manifest. The CLI still applies `.dockerignore`, excludes
`.git`, and uploads exactly one ZIP, but it does not require a Dockerfile:

```json
{"build":"runtime","runtime":"wordpress-tailwind","port":8080}
```

```sh
npm run build
previewctl deploy --manifest preview.json --output json .
```

Open the read-only deployment dashboard at the orchestrator's root URL, such as
`https://api.preview.example.com/`.
The bundled Traefik rule exposes only that exact `/` path. Health and `/v1/*`
remain on the loopback orchestrator port for SSH-forwarded automation.

## Deploy from GitHub Actions

The companion repository is [`dire-kiwi/preview-deployment-action`](https://github.com/dire-kiwi/preview-deployment-action):

```yaml
name: Preview

on:
  pull_request:

permissions:
  contents: read

jobs:
  deploy:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@v6

      - name: Build a Linux executable
        run: CGO_ENABLED=0 GOOS=linux go build -o dist/app ./cmd/server

      - name: Deploy preview
        id: preview
        uses: dire-kiwi/preview-deployment-action@v1
        with:
          endpoint: http://127.0.0.1:8081
          source: dist/app
          manifest: preview.json
          token: ${{ secrets.PREVIEW_DEPLOYMENT_TOKEN }}
          allow-insecure-http: true

      - run: echo '${{ steps.preview.outputs.url }}'
```

The default server binds to loopback, so this example requires a trusted self-hosted runner that can reach the Docker host. A GitHub-hosted runner needs an HTTPS API endpoint reachable from GitHub. Configure authentication and TLS before exposing the API; setting `PUBLIC_SCHEME=https` only changes generated preview URLs and does not terminate TLS.

Each Action run creates a new preview. Rerunning a job does not replace or delete the previous deployment.

## API authentication

Set `API_TOKEN` in the server's `.env`, then run the stack installer again. Every `/v1/*` request will require an exact bearer token; `/healthz` remains public.

```sh
PREVIEWCTL_TOKEN='replace-with-a-secret' previewctl list
```

Authentication is disabled when `API_TOKEN` is empty, preserving local-development behavior. Bearer authentication does not provide encryption: use HTTPS whenever traffic leaves a trusted host or private network.

## ZIP contracts

The original executable form contains:

```text
app              required, at ZIP root
preview.json     optional, at ZIP root; build omitted or "executable"
```

`app` must be a Linux ELF executable for an architecture supported by the Docker host. It must listen on `0.0.0.0:$PORT`; the orchestrator supplies `PORT` from the manifest, defaulting to `8080`. Generated images use `debian:bookworm-slim` and install Bash plus the system CA bundle in a cached layer before copying the application. A custom executable base selected with `RUNTIME_IMAGE` must provide `apt-get` or `apk`, or already contain `/bin/bash` and `/etc/ssl/certs/ca-certificates.crt`.

The Dockerfile form contains:

```text
Dockerfile        required, exactly at ZIP root
preview.json      required, at ZIP root with "build":"dockerfile"
...               regular build-context files and directories
```

The orchestrator validates the expanded context, converts it to a sanitized TAR,
and invokes the Docker Engine on the preview host. It never extracts the upload
onto the host and never pushes the resulting `preview-deployment/<id>:latest`
image to a registry. Paths must be canonical relative POSIX paths; duplicate
entries, links, special files, traversal, oversized contexts, and images that
declare Docker volumes are rejected. Dockerfile builds are for trusted code:
build-time `RUN` instructions have network access and are not a strong
multi-tenant sandbox.

The reusable-runtime form contains:

```text
preview.json      required, with "build":"runtime" and a logical "runtime" key
...               regular application source files and directories
```

The logical key is resolved only through the server's `PREVIEW_RUNTIMES`
allowlist, for example
`wordpress-tailwind=preview-runtime/wordpress-tailwind:7.0.1-v1`. Configured
image references are restricted to the local `preview-runtime/` namespace and
must already exist on the Docker host. The runtime image must provide an
entrypoint that expands `/opt/preview/source.zip` into a writable location such
as `/home/preview`.

The orchestrator never pulls or builds a runtime image. It inspects the allowed
local reference, creates the container by immutable image ID, validates and
canonically repacks the source as one ZIP without host extraction, and bind
mounts that exact file read-only at `/opt/preview/source.zip`. Payload files live
under the root-only `PREVIEW_PAYLOAD_DIR` and survive orchestrator restarts;
deleting a preview first removes its container, then its payload. Runtime images
are shared host assets and are never deleted with a preview.

Example `preview.json`:

```json
{
  "build": "executable",
  "name": "checkout-pr-123",
  "port": 8080,
  "args": ["--log-format=json"],
  "codex_auth": false,
  "env": {
    "APP_ENV": "preview"
  }
}
```

`build` may be `executable`, `dockerfile`, or `runtime`; omitting it preserves
executable behavior. `runtime` is required for runtime mode and forbidden in
the other modes. Runtime mode also rejects `codex_auth`. Unknown manifest
fields, a user-provided `PORT`, links, unsafe ZIP
paths, non-ELF executable files, and oversized archives are rejected.

Setting `"codex_auth": true` opts that deployment into a read-only bind mount
of the host file configured by `CODEX_AUTH_PATH`. The source appears at
`/run/secrets/codex-auth.json`. The non-root entrypoint atomically copies it with
mode `0600` to `$CODEX_HOME/auth.json` on the container's writable tmpfs before
starting the app, allowing token refreshes without making the host credential
writable. Set `CODEX_HOME=/tmp/.codex` or another writable tmpfs path in the
manifest environment.
Only enable this for trusted code because a process inside the preview can read
and transmit the mounted credential.

To package manually:

```sh
CGO_ENABLED=0 GOOS=linux go build -o app ./cmd/my-service
zip deployment.zip app preview.json
curl --fail-with-body -H 'Content-Type: application/zip' \
  --data-binary @deployment.zip \
  http://127.0.0.1:8081/v1/deployments
```

## Lifecycle API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Read-only deployment dashboard |
| `POST` | `/v1/deployments` | Upload, build, create, and start |
| `GET` | `/v1/deployments` | List managed deployments |
| `GET` | `/v1/deployments/{id}` | Inspect one deployment |
| `POST` | `/v1/deployments/{id}/stop` | Stop it |
| `POST` | `/v1/deployments/{id}/start` | Start it again |
| `GET` | `/v1/deployments/{id}/logs?tail=200` | Read combined logs, up to 5,000 requested lines |
| `DELETE` | `/v1/deployments/{id}` | Remove its container and any orchestrator-owned image |
| `GET` | `/healthz` | Check orchestrator and Docker access |

Deploy success means Docker started the container; it does not guarantee that the uploaded application is ready to serve traffic.

## Preview hibernation

Running previews are stopped after 30 minutes without an HTTP request through
Traefik. Their containers and images remain in place. The first request after
hibernation starts the same container and receives a retryable `503` page that
says the preview is resuming; the page refreshes automatically and normal
Traefik routing takes over after a short startup grace period and the
application port is accepting connections.

Hibernation is a Docker stop/start cycle, not process checkpointing. Processes
and in-memory state are lost, and the preview's tmpfs-backed `/tmp` and
`/home/preview` filesystems start empty after every resume. Store anything that
must survive hibernation outside those ephemeral locations. Set the global
`PREVIEW_IDLE_TIMEOUT=0` before deploying a preview when stop/start semantics
are unsuitable; per-preview opt-out is not currently supported.

Traefik provides the stopped-container routing hook, while the orchestrator
owns request tracking and Docker start/stop operations. Wake callbacks carry a
random per-preview control token, and the supplied stack does not expose
Traefik's diagnostic API where dynamic labels could reveal those tokens. A
manual `previewctl stop` is also temporary: the next routed request resumes
that preview.

The stopped-container hook requires Traefik 3.6 or newer; the supplied Compose
file pins a compatible release. Because ForwardAuth observes every request,
otherwise healthy previews temporarily depend on the orchestrator being
available.

Configure the behavior with `PREVIEW_IDLE_TIMEOUT` (the supplied stack defaults
to `30m`) and `PREVIEW_IDLE_CHECK_INTERVAL` (default `30s`). Set
`PREVIEW_IDLE_TIMEOUT=0` to disable both automatic hibernation and
request-triggered resume middleware for newly deployed previews. Existing
containers keep their original immutable Traefik labels. Legacy containers are
never stopped by the hibernator because they have no safe wake route; redeploy
them to opt in.

Upgrade with the complete released Compose stack, not by changing only the
orchestrator image tag. The binary deliberately defaults hibernation off when
`PREVIEW_IDLE_TIMEOUT` is absent, so an image-only upgrade remains safe, but it
does not remove the older stack's diagnostic Traefik API or enable hibernation.

The cold request itself is not replayed. Browsers safely refresh `GET` pages,
but clients sending a cold `POST`, `PUT`, or other non-idempotent request must
retry after the `503` response. ForwardAuth observes a WebSocket, SSE, or other
long-lived connection only when it is opened; use a conservative timeout or
disable hibernation globally when connections may remain quiet longer than the
idle timeout. An orchestrator restart gives already-running previews a
fresh idle interval because last-request times are intentionally kept in
memory rather than in a database.

Downgrading to a release before request-driven hibernation requires recreating
previews deployed by this release. Their immutable Traefik labels reference the
new orchestrator wake callback, which older orchestrators do not serve; without
redeployment, those preview routes return `404` after a rollback even when the
containers are running. Pre-existing legacy previews do not have this label and
are unaffected.

## Local development

Requirements: Docker with Compose and Go 1.24 or newer.

```sh
make up
make test
make deploy-example
make test-hibernation
```

`make up` layers `compose.dev.yaml` over the release Compose file and builds the orchestrator locally. `make logs` follows platform logs. Delete preview deployments before `make down`, because their containers remain attached to the shared `preview-network`.

`make test-hibernation` starts a disposable privileged Docker-in-Docker daemon,
then builds an isolated local image and runs real Traefik, orchestrator,
legacy-preview, and preview containers inside it. It verifies keepalive traffic,
idle stop, the `503` resume page, same-container restart, readiness, and a second
idle cycle, then removes the disposable daemon and all of its resources.

## Configuration

Copy `.env.example` when running from a source checkout. Common settings are:

- `API_TOKEN`: optional bearer token for `/v1/*`.
- `PREVIEW_DOMAIN`: suffix used for preview hostnames.
- `TRAEFIK_HTTP_PORT` and `PUBLIC_PORT`: listening and advertised ports; keep them equal.
- `MAX_DEPLOYMENTS`, `MAX_UPLOAD_MB`, and `BUILD_CONCURRENCY`: platform capacity controls.
- `PREVIEW_IDLE_TIMEOUT`: request-free duration before a preview is stopped;
  `0` disables hibernation. `PREVIEW_IDLE_CHECK_INTERVAL` controls scan frequency.
- `PREVIEW_MEMORY_MB`, `PREVIEW_CPUS`, `PREVIEW_PIDS_LIMIT`, and `PREVIEW_TMPFS_MB`: per-preview limits.
- `CODEX_AUTH_PATH`: optional absolute host path mounted read-only only for
  manifests that explicitly request `codex_auth`.
- `PREVIEW_PAYLOAD_DIR`: absolute root-only host directory mounted at the same
  path in the orchestrator; the installer defaults it to `INSTALL_DIR/payloads`.
- `PREVIEW_RUNTIMES`: comma-separated logical runtime keys mapped to trusted
  local `preview-runtime/` image references.
- `PREVIEW_DEPLOYMENT_VERSION`: orchestrator image tag used by Compose.

## Runtime isolation and security boundary

Generated, Dockerfile-built, and reusable-runtime preview containers run as
UID/GID `65534`, with a read-only root filesystem, a no-exec ephemeral `/tmp`,
an executable ephemeral `/home/preview` workspace, all Linux capabilities dropped,
`no-new-privileges`, no host ports, and CPU, memory, PID, log-size, and
deployment-count limits. Dockerfile and runtime images retain their entrypoint
and working directory but cannot declare volumes. Runtime payloads use the
managed read-only file bind described above; the only user-selectable host mount
is the explicit read-only Codex auth opt-in described above.

The platform still executes uploaded code. The orchestrator's Docker socket access is effectively host-root access even though the socket mount is read-only. Before allowing untrusted users or remote traffic, use a dedicated host or isolated Docker daemon, terminate TLS, set `API_TOKEN`, restrict egress, and consider a tightly scoped Docker socket proxy. Preview filesystems are ephemeral; application data and volumes are not persisted.
