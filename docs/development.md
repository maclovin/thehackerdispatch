# Development

## Run locally

Both ways below use the PostgreSQL from `compose.yaml`, so they share the same database. Don't run both at once: each takes port 8080.

### In containers

```sh
make up                          # PostgreSQL + app at http://localhost:8080
CONTAINER_TOOL=podman make up    # same, with Podman
docker compose exec app /thehackerdispatch master-password    # in another terminal
```

`compose.yaml` already sets the required variables for the app container (`DATABASE_URL`, `BASE_URL`, a development-only `SECRETS_KEY`, `LOG_LEVEL=debug`). To try Slack or SSO, add `SLACK_BOT_TOKEN` or the `OIDC_*` variables there.

Ctrl+C stops the stack and keeps the containers. `make down` also removes them. The data stays in the `db-data` volume; `docker compose down -v` erases it.

On Linux, if `make up` fails with `permission denied while trying to connect to the docker API`, add your user to the `docker` group (`sudo usermod -aG docker $USER`) and log in again.

### With Go

Quicker when changing code: only PostgreSQL runs in a container. Needs Go 1.27.

```sh
make db      # PostgreSQL only, on localhost:5432
make run     # the app at http://localhost:8080
go run ./cmd/thehackerdispatch master-password    # in another terminal
```

`make run` sets the same development values as `compose.yaml`. The `SECRETS_KEY` must match because both ways share the database, and the LLM API keys saved in the panel are encrypted with it. To try Slack or SSO, export `SLACK_BOT_TOKEN` or the `OIDC_*` variables before `make run`.

### The master password locally

The master account is created once per database, and only the process that created it has the password file, which is deleted on the first login. So `master-password` says there's no file (or shows an old password) if you already logged in, if the other way created the account, or if the container was removed. Reset it as in [Lost master password](deployment.md#lost-master-password).

## Make targets

| Target | What it does |
|---|---|
| `make build` | Binary in `bin/` |
| `make test` | All tests; database tests are skipped without `make db` |
| `make lint` | `gofmt`, `go vet`, and `golangci-lint` if installed |
| `make db` | Start only PostgreSQL |
| `make run` | Run the app with Go against `make db` |
| `make up` / `make down` | Start / remove the local stack |
| `make image` / `make push` | Build / push the container image |
| `make k8s-render` | Render the example Kubernetes overlay |

Database tests run against a real Postgres (`TEST_DATABASE_URL`, which `make test` points at `make db`); each test gets a throwaway migrated database.

## Continuous integration

- `.github/workflows/ci.yml` runs on every push to `main` and every pull request: `gofmt`, `go vet`, `go test -race` against a PostgreSQL service, and a build of the container image (not pushed).
- `.github/workflows/release.yml` runs on `vX.Y.Z` tags: the same tests, then it pushes the image to GHCR and creates the GitHub release. See [Releases](deployment.md#releases).

## Repository layout

The layout follows the official Go guide ["Organizing a Go module"](https://go.dev/doc/modules/layout): commands in `cmd/`, all application code in `internal/`, no `pkg/`.

```
cmd/thehackerdispatch/  wiring only; subcommands: serve, migrate, master-password, version
internal/
  config/        environment variables → Config struct
  store/         pgx pool and migrations; each feature package owns its own SQL
    migrations/  *.sql files (goose), embedded
    storetest/   throwaway migrated database for tests
  web/           HTTP handlers, routes, middleware
    templates/   html/template files, embedded
    static/      CSS and htmx.min.js, embedded
  auth/          OIDC SSO, local login, sessions, roles
  ingest/        feed fetching and parsing, cursors
  curate/        curation call and decision handling
  pacing/        slot planner and statistics
  publish/       writing and publishing
  notify/        outbox and Slack client
  discuss/       comments and votes
  reaper/        expiration
  llm/           provider interface and adapters, fallbacks
  fetch/         safe HTTP client (anti-SSRF)
  settings/      runtime settings stored in the database
  prompts/       editable prompts, global and per section, versioned
  sources/       sections, sources, OPML import
  jobs/          periodic jobs: one replica at a time
  audit/         audit log entries
  secretbox/     AES-256-GCM for the LLM API keys
deploy/          Kustomize base and example overlay
docs/            documentation, plus sources.opml (feed catalog)
```

`go:embed` can only reach files at or below the embedding package's directory, which is why templates and static files live under `internal/web/` and migrations under `internal/store/`.

## Conventions

| Topic | Convention |
|---|---|
| Layout | No `pkg/`, no `src/`, and no catch-all `utils`/`common`/`models` packages |
| Packages | One package per concept, named after what it provides, with no stutter (`ingest.Run`, not `ingest.IngestRun`) |
| Entry point | `main` only calls `run(ctx, args, getenv, stdout) error` |
| Wiring | Explicit, through constructors. No DI framework, no package-level globals, no `init()` side effects |
| HTTP | Standard library `net/http` with Go 1.22+ routing patterns (`GET /s/{id}`). No web framework |
| Database | `pgx/v5` (`pgxpool`) with plain SQL; migrations with `goose`, embedded in the binary |
| Interfaces | Small, and defined where they're consumed (e.g. `llm.Provider`) |
| Context | `context.Context` as the first parameter of anything that does I/O |
| Errors | Wrapped with `fmt.Errorf("...: %w", err)`, checked with `errors.Is`/`errors.As` |
| Logging | `log/slog`, JSON to stdout |
| Concurrency | `errgroup` runs the web server and the workers; `signal.NotifyContext` drives shutdown |
| Config | Environment variables parsed once in `internal/config`; everything else is a runtime setting in the database |
| Tests | Standard `testing`, table-driven, `httptest`. Database tests skip when `TEST_DATABASE_URL` isn't set |

## Libraries

| Purpose | Library |
|---|---|
| Postgres | `pgx/v5` |
| Migrations | `goose` (embedded) |
| Concurrency | `golang.org/x/sync/errgroup` |
| Feeds | `gofeed` |
| OIDC | `go-oidc` + `x/oauth2` |
| Article extraction | `go-readability` |
| Markdown | `goldmark` + `bluemonday` |
| LLM | `anthropic-sdk-go`, `openai-go` (also for compatible APIs, Gemini included) |
| Slack | Direct HTTP API (`net/http`) |
