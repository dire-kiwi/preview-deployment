# Preview deployment

A small, self-hosted preview platform built from two long-running containers:

- **Traefik** discovers preview containers from Docker labels and routes each deployment by hostname.
- **Orchestrator** accepts a ZIP containing a Linux executable and owns its build, create, start, stop, logs, and delete lifecycle.

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
| Traefik dashboard | `http://127.0.0.1:8082` |

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

## ZIP contract

An uploaded ZIP contains:

```text
app              required, at ZIP root
preview.json     optional, at ZIP root
```

`app` must be a Linux ELF executable for an architecture supported by the Docker host. It must listen on `0.0.0.0:$PORT`; the orchestrator supplies `PORT` from the manifest, defaulting to `8080`. Generated images use `debian:bookworm-slim` and install Bash plus the system CA bundle in a cached layer before copying the application. Custom runtime images must provide `apt-get` or `apk`, or already contain `/bin/bash` and `/etc/ssl/certs/ca-certificates.crt`.

Example `preview.json`:

```json
{
  "name": "checkout-pr-123",
  "port": 8080,
  "args": ["--log-format=json"],
  "codex_auth": false,
  "env": {
    "APP_ENV": "preview"
  }
}
```

Unknown manifest fields, a user-provided `PORT`, links, unsafe ZIP paths, non-ELF files, and oversized archives are rejected.

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
| `POST` | `/v1/deployments` | Upload, build, create, and start |
| `GET` | `/v1/deployments` | List managed deployments |
| `GET` | `/v1/deployments/{id}` | Inspect one deployment |
| `POST` | `/v1/deployments/{id}/stop` | Stop it |
| `POST` | `/v1/deployments/{id}/start` | Start it again |
| `GET` | `/v1/deployments/{id}/logs?tail=200` | Read combined logs, up to 5,000 requested lines |
| `DELETE` | `/v1/deployments/{id}` | Remove its container and image |
| `GET` | `/healthz` | Check orchestrator and Docker access |

Deploy success means Docker started the container; it does not guarantee that the uploaded application is ready to serve traffic.

## Local development

Requirements: Docker with Compose and Go 1.24 or newer.

```sh
make up
make test
make deploy-example
```

`make up` layers `compose.dev.yaml` over the release Compose file and builds the orchestrator locally. `make logs` follows platform logs. Delete preview deployments before `make down`, because their containers remain attached to the shared `preview-network`.

## Configuration

Copy `.env.example` when running from a source checkout. Common settings are:

- `API_TOKEN`: optional bearer token for `/v1/*`.
- `PREVIEW_DOMAIN`: suffix used for preview hostnames.
- `TRAEFIK_HTTP_PORT` and `PUBLIC_PORT`: listening and advertised ports; keep them equal.
- `MAX_DEPLOYMENTS`, `MAX_UPLOAD_MB`, and `BUILD_CONCURRENCY`: platform capacity controls.
- `PREVIEW_MEMORY_MB`, `PREVIEW_CPUS`, `PREVIEW_PIDS_LIMIT`, and `PREVIEW_TMPFS_MB`: per-preview limits.
- `CODEX_AUTH_PATH`: optional absolute host path mounted read-only only for
  manifests that explicitly request `codex_auth`.
- `PREVIEW_DEPLOYMENT_VERSION`: orchestrator image tag used by Compose.

## Runtime isolation and security boundary

Generated preview containers run as UID/GID `65534`, with a read-only root filesystem, a no-exec ephemeral `/tmp`, an executable ephemeral `/home/preview` workspace, all Linux capabilities dropped, `no-new-privileges`, no host ports, and CPU, memory, PID, log-size, and deployment-count limits. The only supported host mount is the explicit read-only Codex auth opt-in described above.

The platform still executes uploaded code. The orchestrator's Docker socket access is effectively host-root access even though the socket mount is read-only. Before allowing untrusted users or remote traffic, use a dedicated host or isolated Docker daemon, terminate TLS, set `API_TOKEN`, restrict egress, protect the Traefik dashboard, and consider a tightly scoped Docker socket proxy. Preview filesystems are ephemeral; application data and volumes are not persisted.
