# Deployment

The only required infrastructure is one PostgreSQL 16+ database. The app ships as a single container image.

## Container image

### Releases

Versions follow [semantic versioning](https://semver.org) and come only from git tags. Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`, which:

1. checks that the tag is a new version (see below);
2. runs the tests;
3. builds the image for `linux/amd64` and `linux/arm64` and pushes it to `ghcr.io/maclovin/thehackerdispatch`;
4. creates the GitHub release, with notes generated from the merged pull requests and the image name.

| Tag | Image tags | GitHub release |
|---|---|---|
| `v1.2.3` | `1.2.3`, `1.2`, `latest` | regular release |
| `v1.2.3-rc.1` | `1.2.3-rc.1` | pre-release |

Nothing else publishes: pushes to `main` and pull requests only run the tests and check that the image builds.

To release, create a new tag (e.g. `v1.2.3`) on a commit of `main` that passed CI, in the GitHub UI or with `git tag v1.2.3 && git push origin v1.2.3`. The workflow creates the release, with generated notes and the image name. If a release for that tag already exists, it only adds the image line to its notes.

The workflow stops without publishing an image if:

- the tag isn't a version (`vX.Y.Z` or `vX.Y.Z-suffix`);
- an image for that version is already in GHCR. Published versions are never overwritten;
- the tests or the image build fail.

When it stops, a release that already exists for the tag goes back to **draft**, so no published release lacks its image. Fix the problem and create a new tag (e.g. `v1.2.4`).

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

### Secrets from an external manager

The app reads its secrets from environment variables, filled from a Kubernetes Secret. Any tool that keeps that Secret in sync works: [External Secrets Operator](https://external-secrets.io) (OpenBao, Vault, AWS, GCP, Azure, and others), or the Secrets Store CSI Driver with Secret sync.

With ESO:

1. Remove the `secretGenerator` from the overlay's `kustomization.yaml`, and `secrets.env` with it.
2. Create an `ExternalSecret` whose target is a Secret named `thehackerdispatch-secrets`, with the same keys as `secrets.env.example`:

   ```yaml
   apiVersion: external-secrets.io/v1
   kind: ExternalSecret
   metadata:
     name: thehackerdispatch-secrets
     namespace: thehackerdispatch
   spec:
     refreshInterval: 1h
     secretStoreRef:
       kind: ClusterSecretStore
       name: openbao              # your store
     target:
       name: thehackerdispatch-secrets
     dataFrom:
       - extract:
           key: thehackerdispatch # path in OpenBao/Vault holding DATABASE_URL, SECRETS_KEY, ...
   ```

3. Restart the pods when the Secret changes. Environment variables are read only at startup, so a resync that changes a value has no effect until then. [Reloader](https://github.com/stakater/Reloader) does it: install it and annotate the Deployment:

   ```yaml
   metadata:
     annotations:
       secret.reloader.stakater.com/reload: "thehackerdispatch-secrets"
   ```

A resync then causes no downtime:

- the Deployment starts the new pod and waits until it is ready before stopping the old one (`maxUnavailable: 0`);
- if the new values are wrong (bad `DATABASE_URL`, invalid `SECRETS_KEY`), the new pod never becomes ready and the old one keeps serving. `kubectl rollout status` shows the stuck rollout;
- the old pod stops cleanly: it fails `/readyz`, waits 5s for Kubernetes to stop routing to it, then drains its connections.

> **Don't rotate `SECRETS_KEY`.** It encrypts the LLM API keys stored in the database. If a resync changes it, the new pod starts fine but can no longer decrypt those keys: curation and writing fail until the keys are entered again in the panel. Rotate the other secrets freely.

## Operations

- **Probes:** liveness on `/healthz` (the process is up); readiness on `/readyz` (the database is reachable and migrations have run).
- **Shutdown:** on `SIGTERM` the process stops taking new jobs, fails `/readyz`, waits 5s so Kubernetes stops routing to it, then drains HTTP connections, all within the default 30s grace period.
- **Rollouts:** `maxUnavailable: 0`, so a restart (new release, changed config or secret) keeps the old pod serving until the new one is ready.
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
