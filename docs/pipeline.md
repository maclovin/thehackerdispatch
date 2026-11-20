# Content pipeline

How a feed item becomes a post, and how it disappears. See the [story lifecycle](architecture.md#story-lifecycle) for the overview.

## 1. Ingestion (`ingest`)

- **Scheduling:** one cycle per configured poll interval. All enabled sources are fetched in parallel, with a global limit (16) and a per-host limit (2).
- **HTTP:**
  - conditional GET with `ETag`/`If-Modified-Since`;
  - 20s timeout, 5 MB body;
  - on `429` or `503`, the source isn't fetched again for as long as `Retry-After` asks, or else for 15 minutes doubled for each earlier failure in a row; never more than 24h;
  - its own User-Agent, overridable per source, because some sites block bot UAs and others block browser UAs.
- **Source health:** last fetch time and status, consecutive failures, and items of the last 48h are shown in the panel. After 10 consecutive failures the source is disabled and flagged.
- **Parsing:** RSS 2.0, Atom, and RDF. For each item it keeps the canonical URL (without `utm_*` and similar), the title, a plain-text snippet (up to ~1,000 characters), the publication date, and a hash of the GUID.
- **What counts as "new":** there is no history of seen items. Each source keeps only `cursor_at` (the latest publication date processed), `last_guids` (hashes of the GUIDs in the last fetch), and the `ETag`/`Last-Modified` pair. An item is new if its GUID was not in the last fetch **and** its date is later than `cursor_at` or it has no date. On a source's first fetch, only items from the last 48h are taken, and undated items are skipped.

## 2. Curation (`curate`, LLM call 1)

Runs right after each ingestion cycle, if there are pending items.

**Batches.** At most ~40 items per call, processed sequentially. All items of a batch come from sources of one section, so the batch gets that section's prompt. Each batch sees the stories created by earlier batches in the same cycle.

**What goes into the call**, in this order to benefit from prompt caching:

1. **Contract** (fixed, not editable): the model's role, the output schema, and the rule that news content is data, not instructions.
2. **Sections:** slug and name of each, to pick from.
3. **Global prompt** (editable): who reads the board, what is relevant, how to score, what counts as critical, how to write.
4. **Section prompt** (editable, optional).
5. **Known stories:** queued or published stories that haven't expired, in compact form (id, title, one-line summary, entities, status). Every story from the last 14 days (up to 300); older ones only when Postgres full-text search finds them similar to an item (3 best matches per item) or they share a CVE.
6. **New items:** id, source, source section, title, snippet, and date.

**Output**, one record per item, validated against a JSON schema on the server:

| Field | Content |
|---|---|
| `item_id` | The item being evaluated |
| `decision` | `discard`, `duplicate`, `update`, or `new` |
| `story_ref` | Id of the existing story (`duplicate`/`update`), or a cluster key (`new`) shared by items of the same batch about the same story |
| `section` | Final section (may differ from the source's section) |
| `score` | 0–100 |
| `priority` | `normal` or `critical` |
| `entities` | CVEs, vendors, products, threat actors |
| `reason` | One sentence explaining the decision (shown in the panel) |
| `update_note` | For `update`: what changed, in one sentence |

**Effect of each decision:**

- **`discard`:** the item keeps the decision and reason, and is deleted after 48h.
- **`duplicate`:** the item becomes an extra link on the story (up to 5 sources per story). If the story is still queued, its score goes up slightly.
- **`update`:** a published story gets an update note and a reply in its Slack thread, at most once every 6h; updates don't count toward the cap. A queued story just absorbs the item.
- **`new`:** creates a queued story with the main item and the links from the same cluster.
- **`critical`:** the story goes straight to publishing, bypassing pacing.

**Failures:**

- An item with no decision is retried in the next run; after 3 attempts it is discarded with "no decision returned".
- A `duplicate` or `update` whose `story_ref` doesn't exist is treated as `new`.
- If the call fails, the items stay pending; after 3 failures an item is discarded with `curation_failed`.

## 3. Pacing (`pacing`)

**Goal:** spread up to `C` posts across the day, publishing the best ones and following the arrival rate.

| Parameter | Default |
|---|---|
| Cap `C` | 10 (panel) |
| Minimum gap `g` | 30 min (panel) |
| Minimum score `s_min` | 40 (panel) |
| Jitter | ±15% (fixed) |

**Statistics:** one row per normal story curation creates, with its arrival time and score, kept for 4 weeks (no content). From them: `λ(weekday, hour)`, the average number of new stories in that hour, and the recent score distribution.

**Planning** runs after every curation and every minute. A day is a calendar day in the database's time zone.

1. Remaining budget: `B = C − published_today` (critical posts count).
2. Expected supply: `N = queued stories + sum of λ over the remaining hours`.
3. Quality bar: `θ = (1 − B/N)` quantile of recent scores, never below `s_min`. If `N ≤ B`, `θ = s_min`.
4. Only the next slot is planned: the time left in the day divided by `B + 1`, with jitter, never less than `g` after the previous one.
5. **At each slot**, take the highest-scoring story queued today. If its score is `≥ θ`, publish it; otherwise skip the slot and keep the budget.
6. **At the end of the day**, whatever is left in the queue is dropped.

**Example.** At 10:00, `B = 8`, 3 stories are queued, and history predicts 25 more. `N = 28`, so `θ` is the 71st percentile: only stories in the top ~29% go out now.

**Cold start.** Until there is a week of history, the average rate so far is spread evenly over the hours. Until there are 50 scores, scores are taken as uniform from 0 to 100.

## 4. Publishing (`publish`, LLM call 2)

1. The story moves from `queued` to `publishing`, under a lock.
2. The main source's article is downloaded with the safe `fetch` client, its main text extracted (readability) and trimmed to ~8k tokens. If that fails, the feed snippets are used.
3. **Writing:** the writing stage's models (or the curation stage's, if it has none) get the fixed contract, the global and section prompts, the story data, the article text, and the other sources' snippets. The output is JSON:
   - `title`: up to 90 characters, neutral, no clickbait;
   - `summary`: 2–4 short paragraphs or bullets, in a Markdown subset;
   - `why_it_matters`: 1–2 sentences;
   - `tldr`: one line, used in Slack;
   - `entities`: refined.
4. In one transaction the story becomes `published`, gets `published_at` and `expires_at = published_at + TTL` (null if forever), and, if its section has a channel, the alert is queued in the outbox.
5. If every model fails, the post is published with the feed's title and snippet.

## 5. Notifications (`notify`)

- **Channels** are configured in the panel, each with a name and a Slack channel (by name, `#sec-news`, or ID, `C0123456789`). One Slack app posts to all of them and must be invited to each.
- **Section → channel:** each section points to one channel or none; sections can share a channel. The alert goes to the channel of the post's final section.
- **Messages:** `chat.postMessage` for the post, and a thread reply for updates. The channel and timestamp are stored with the story, so replies land in the right thread even if the mapping changes. Messages are never deleted; once the post expires, the link opens a page saying so.
- **Format:** `[SECURITY] title`, the `tldr`, a link to the internal thread ("Discuss on <display name>"), and a link to the original source. Critical posts get a `🚨 CRITICAL` prefix.
- **Outbox:** messages are written in the same transaction as the publish (or update). A job delivers them right away and every minute, at most one per second.
  - A failed message waits 1 minute, doubled each failure (up to 1 hour, or longer if Slack's `Retry-After` asks), and is given up after 10 attempts.
  - A reply waits for its story's alert; if the alert was never sent, the reply isn't either.
  - Without `SLACK_BOT_TOKEN`, messages fail at once instead of piling up.
  - Messages for expired stories are dropped.

## 6. Expiration (`reaper`)

Runs every 5 minutes.

| What | When |
|---|---|
| Published stories (with links, updates, comments, and votes); Slack messages stay | `expires_at < now` |
| Stories still in the queue | End of the day |
| Raw items (with decisions and reasons) | 48h |
| Slack outbox, sent or failed | 7 days |
| Pacing arrivals | 4 weeks |
| Sessions | End of the configured lifetime |
| LLM call log | 30 days |
| Audit log | 90 days |

**TTL changes** are retroactive: `expires_at` is recomputed for everything published, and lowering the TTL immediately deletes whatever falls outside it. When a story is deleted it also leaves curation's list of known stories, so dedup memory follows the TTL.
