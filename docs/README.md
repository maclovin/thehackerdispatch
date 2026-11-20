# Documentation

## Guides

| Document | What it covers |
|---|---|
| [Getting started](getting-started.md) | First-time setup in the admin panel: domains, sources, prompts, LLM, Slack |
| [Configuration](configuration.md) | Environment variables, and what is configured in the panel instead |
| [Deployment](deployment.md) | Container image, Kubernetes, probes, scaling, backups, lost master password |
| [Development](development.md) | Running locally, tests, repository layout, Go conventions, libraries |

## Design

| Document | What it covers |
|---|---|
| [Architecture](architecture.md) | Principles, components, run modes, story lifecycle |
| [Content pipeline](pipeline.md) | Ingestion, curation, pacing, publishing, notifications, expiration |
| [LLM integration](llm.md) | Providers, models and stages, structured output, fallbacks |
| [Authentication](authentication.md) | SSO and local accounts, master account, sessions, roles |
| [Portal and admin panel](portal.md) | Board, story pages, comments, votes, themes, branding, admin panel |
| [Security](security.md) | Prompt injection, SSRF, XSS, CSRF, passwords, secrets |
| [Data model](data-model.md) | Tables, columns, and retention |

## Resources

| File | What it is |
|---|---|
| [sources.opml](sources.opml) | Catalog of 39 high-signal security RSS/Atom feeds, ready to import in the admin panel (sources → Import OPML). See [Getting started](getting-started.md) |
