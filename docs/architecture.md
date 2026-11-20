# Architecture

An internal news portal in the style of Hacker News, focused on security. Humans pick the sources. An LLM does all the curation: it decides what gets in, deduplicates, scores, sets priority, and writes the posts. People read and discuss in threads, voting on comments. Everything has an expiration date.

## Principles

1. **100% AI curation.** No human approves or writes posts. Admins configure sources, prompts, and parameters, and they can delete content, but they don't publish.
2. **Fresh by design.** All content expires according to a global TTL. When something expires it is really gone: the post, its thread, the votes, and the dedup memory for that story. Slack messages are the exception: they are never deleted.
3. **Anti-distraction.** A daily cap on posts, with publishing spread across the day. The portal should take about 15 minutes a day to read.
4. **Simple to run.** One Go binary and one PostgreSQL. No external queue, no external cache, no microservices.

## Components

A single binary with internal modules. All state lives in PostgreSQL.

```
   RSS/Atom feeds                     LLM API                          Slack (channels)
         │                     ┌─────────┴──────────┐                        ▲
         ▼                     │                    │                        │
   ┌───────────┐         ┌─────┴──────┐       ┌─────┴──────┐   ┌────────────┴─┐
   │  ingest   │────────►│  curate    │──┬───►│  publish   │──►│    notify    │
   └───────────┘         └────────────┘  │    └─────▲──────┘   │ (via outbox) │
                                         │          │          └──────────────┘
                                  normal │          │ at slot time
                                         ▼          │
                                  ┌────────────┐  ┌─┴──────────┐
                                  │ daily queue│─►│   pacing   │
                                  └────────────┘  └────────────┘
                      (critical stories go straight from curate to publish)

   ┌──────────────────────────────────────────┐   ┌────────────┐
   │ web: board · threads · votes · admin     │   │   reaper   │  expires everything by TTL
   └──────────────────────────────────────────┘   └────────────┘
          ▲
   Users (OIDC SSO or local login)
```

| Module | Responsibility |
|---|---|
| `ingest` | Polls sources at the configured interval, parses RSS/Atom, and stores new items |
| `curate` | Sends new items to the LLM and applies the decisions: discard, duplicate, update, or new story |
| `pacing` | Keeps the daily queue and decides when each story is published |
| `publish` | Fetches the article, asks the LLM to write the post, publishes it, and enqueues the alert |
| `notify` | Delivers each alert to the section's Slack channel, from an outbox, with retries |
| `reaper` | Deletes whatever has expired |
| `web` | Board, thread pages, votes, comments, and admin panel (server-rendered HTML) |
| `auth` | SSO and local login, password generation, master account bootstrap, sessions, roles |
| `llm` | One LLM interface with an adapter per provider; fallbacks, schemas, refusals |
| `fetch` | Safe HTTP client shared by ingest and publish (limits, anti-SSRF) |

Each stage is described in [Content pipeline](pipeline.md).

## Run modes

`serve --mode=all` (default, everything in one process), `web`, or `worker`. The background jobs (ingest with curation, pacing with publishing, notify, reaper) each take a Postgres advisory lock, so each runs on one replica at a time. Several replicas can run without duplicated work, and the `web` part scales horizontally.

## Story lifecycle

```
 feed item
     │
     ▼
 raw_item (pending) ──► curation ──┬──► discard ──────────► reason recorded; gone after 48h
                                   ├──► duplicate ────────► becomes an extra link on the existing story
                                   ├──► update ───────────► update note on the post + Slack thread reply
                                   └──► new ──► story (queued) ──┬──► published ──► deleted at TTL
                                                    │            └──► dropped at end of day ──► deleted
                                                    └── critical ──► published immediately
```

## What runs where

- **In the binary:** job scheduling (with advisory locks), concurrency limits, templates, and static assets (`go:embed`), migrations.
- **In Postgres:** the Slack outbox, sessions, login throttling, and dedup search (full-text).
- **Not needed:** Redis, a message queue, a search engine, object storage, or volumes.

## See also

- [Content pipeline](pipeline.md)
- [LLM integration](llm.md)
- [Authentication](authentication.md)
- [Portal and admin panel](portal.md)
- [Security](security.md)
- [Data model](data-model.md)
