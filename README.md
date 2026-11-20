# The Hacker Dispatch

Your internal hub for external news, advisories, and key publications.

## Features

- **AI curation.** An LLM reads RSS/Atom feeds, discards noise, merges duplicates, scores each story, and writes the post. No human approves or writes posts.
- **Paced publishing.** A daily cap, spread across the day, best stories first. Critical stories go out right away.
- **Discussion.** Threaded comments, ranked by votes.
- **Fresh by design.** Every post, thread, and vote is deleted after a configurable time.
- **Slack alerts** per section, with updates as thread replies.
- **Any LLM provider:** Anthropic, OpenAI, or any OpenAI-compatible API (Gemini included), with fallbacks.
- **Sign-in** with SSO through OpenID Connect (tested with Keycloak), or local accounts restricted to company email domains.
- **White-label:** display name and colors, light and dark themes.
- **Simple to run:** one Go binary and one PostgreSQL database. No queue, no cache, no extra services.

## Quick start

Requirements: Docker (or Podman) and `make`.

```sh
make up    # PostgreSQL + the app at http://localhost:8080
```

In another terminal, read the master password and sign in as `admin`:

```sh
docker compose exec app /thehackerdispatch master-password
```

Then follow [Getting started](docs/getting-started.md) to set the allowed email domains, add sources, and connect an LLM provider.

## Documentation

Everything is in [`docs/`](docs/README.md):

- [Getting started](docs/getting-started.md): first-time setup in the admin panel
- [Configuration](docs/configuration.md): environment variables
- [Deployment](docs/deployment.md): container image, Kubernetes, operations
- [Development](docs/development.md): running locally, tests, code conventions
- [Architecture](docs/architecture.md): how the system is designed

## Development

Needs Go 1.27 and Docker (or Podman) for PostgreSQL.

```sh
make db      # start only PostgreSQL
make run     # run the app with Go
make test    # all tests; database tests are skipped without `make db`
make lint    # gofmt, go vet, golangci-lint (if installed)
make help    # all targets
```

See [Development](docs/development.md) for details.
