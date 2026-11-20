# Deployment

The only required infrastructure is one PostgreSQL 16+ database. The app ships as a single container image.

## Container image

### Releases

Versions follow [semantic versioning](https://semver.org) and come only from git tags. Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`, which:

1. runs the tests;
2. builds the image for `linux/amd64` and `linux/arm64` and pushes it to `ghcr.io/maclovin/thehackerdispatch`;
3. creates the GitHub release, with notes generated from the merged pull requests and the image name.

| Tag | Image tags | GitHub release |
|---|---|---|
| `v1.2.3` | `1.2.3`, `1.2`, `latest` | regular release |
| `v1.2.3-rc.1` | `1.2.3-rc.1` | pre-release |

Nothing else publishes: pushes to `main` and pull requests only run the tests and check that the image builds.

To release, tag a commit on `main` that passed CI:

```sh
git tag v1.2.3
git push origin v1.2.3
```

The version is built into the binary: `thehackerdispatch version`, `/healthz`, and the startup log all report it.

Deploy exact version tags (`1.2.3`), not `1.2` or `latest`. The Deployment doesn't set `imagePullPolicy`, so a node that already has a moving tag cached keeps running the old image.

The published image is public, so any cluster can pull it without credentials.

> **Maintainers, once:** GHCR creates every new package as private, even for a public repository. After the first release, open the package on GitHub → **Package settings** → **Danger Zone** → **Change visibility** → **Public**. This can't be undone, and it isn't needed again for later releases.

If you publish to a private registry of your own instead (`make push IMAGE=...`), create a pull secret in the namespace and reference it from the Deployment with `imagePullSecrets`.

### Built by hand

```sh
make image IMAGE=registry.example.com/thehackerdispatch VERSION=1.0.0
make push  IMAGE=registry.example.com/thehackerdispatch VERSION=1.0.0
```

Both work with Docker or Podman (`CONTAINER_TOOL=podman`). This builds for the local machine's architecture only.

### Contents

- Multi-stage `Dockerfile`: a static binary (`CGO_ENABLED=0`, cross-compiled per platform) on `distroless/static:nonroot`.
- No shell or package manager, runs as non-root, includes CA certificates. About 65 MB, mostly the Anthropic and OpenAI SDKs.
- The image is tagged with the version and git commit; the binary reports both at `/healthz` and in its startup log.
- Migrations are embedded and applied on startup, followed by the master account bootstrap.

The binary's subcommands are `serve` (default; `--mode=all|web|worker`), `migrate`, `master-password`, and `version`.

## Kubernetes

The manifests in `deploy/` use Kustomize, which is built into `kubectl`. PostgreSQL isn't included: point `DATABASE_URL` at a database you already run (managed, operator, or VM).

- **Base:** a `Deployment` (1 replica, `--mode=all`) and a `Service`.
- **Example overlay:** adds a `Namespace` and an `Ingress`, sets the image, and generates the `ConfigMap` and `Secret` the Deployment reads, from two files:
  - `config.env` → ConfigMap `thehackerdispatch-config`, for settings that aren't secret. It can be committed.
  - `secrets.env` → Secret `thehackerdispatch-secrets`. It must not be committed (`.gitignore` already skips it).

  Both files take one `KEY=value` per line, without quotes (a quote becomes part of the value). An empty value counts as not set. [Configuration](configuration.md) says which variable goes in which file.

### Steps

1. Copy the example overlay (`prod` is just a name):

   ```sh
   cp -r deploy/overlays/example deploy/overlays/prod
   ```

2. In `kustomization.yaml`, set `newTag` to the release you want (e.g. `1.2.3`); change `newName` only if you publish the image elsewhere. In `ingress.yaml`, set the host, plus `ingressClassName` and TLS if the cluster needs them.
3. In `config.env`, set `BASE_URL` to the portal's public URL, with the same host as the Ingress. For SSO, uncomment `OIDC_ISSUER_URL` and `OIDC_CLIENT_ID`.
4. Create `secrets.env` and fill it in:

   ```sh
   cp deploy/overlays/prod/secrets.env.example deploy/overlays/prod/secrets.env
   openssl rand -base64 32    # the value for SECRETS_KEY
   ```

5. Check what will be created, apply it, and read the master password:

   ```sh
   kubectl kustomize deploy/overlays/prod     # prints the manifests, applies nothing
   kubectl apply -k deploy/overlays/prod
   kubectl -n thehackerdispatch rollout status deploy/thehackerdispatch
   kubectl -n thehackerdispatch exec deploy/thehackerdispatch -- /thehackerdispatch master-password
   ```

Then continue with [Getting started](getting-started.md).

To change a value later, edit `config.env` or `secrets.env` and run `kubectl apply -k` again. Kustomize adds a hash of the contents to the ConfigMap and Secret names, so the pods restart with the new values on their own.

## Operations

- **Probes:** liveness on `/healthz` (the process is up); readiness on `/readyz` (the database is reachable and migrations have run).
- **Shutdown:** on `SIGTERM` the process stops taking new jobs, finishes the current ones, and drains HTTP connections, within the default 30s grace period.
- **Security context:** `runAsNonRoot`, all capabilities dropped, no privilege escalation. The root filesystem stays writable because the master password file lives in `/tmp`.
- **Resources:** small defaults (100m CPU, 128Mi, up to 512Mi). Concurrent password checks are capped, so memory stays predictable.
- **Scaling:** 1 replica by default. More replicas are safe: background jobs and migrations run under Postgres advisory locks. The `web` mode scales horizontally; see [run modes](architecture.md#run-modes).
- **Logs:** JSON (`slog`) to stdout, including one line per pacing slot (queue, forecast, bar, next slot).
- **Backups:** expired content still exists in backups until they rotate. For "really gone" to hold, keep backup retention short (e.g. 7 days) and well known.

## Lost master password

The password file lives in the container's `/tmp`, so it is gone once the container is recreated, while the account stays. Delete the master account and restart; a new one is created with a new password:

```sql
DELETE FROM users WHERE is_master;
```

On Kubernetes, run that against the database, then `kubectl -n thehackerdispatch rollout restart deploy/thehackerdispatch` and read the new password.

Locally, with `make up`:

```sh
docker compose exec db psql -U thehacker -d thehackerdispatch -c 'DELETE FROM users WHERE is_master;'
docker compose restart app
docker compose exec app /thehackerdispatch master-password
```

Locally, with `make db` and `make run`: run the same `DELETE`, restart `make run`, then `go run ./cmd/thehackerdispatch master-password`.

This needs database access, which already means full control of the system, so it adds no new risk.
