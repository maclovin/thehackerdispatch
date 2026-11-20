# Data model

All state lives in PostgreSQL. Migrations are in `internal/store/migrations/` (goose), embedded in the binary and applied on startup.

| Table | Main columns | Retention |
|---|---|---|
| `users` | id, auth_mode (`sso`/`local`), email (unique; null only for the master), sso_subject, password_hash, password_temp_expires_at, must_rotate, failed_logins, locked_until, is_master, real_name, nickname, role, preferred_sections, theme, banned_at, created_at, last_login_at | While the user exists |
| `sessions` | id_hash, user_id, ip, user_agent, created_at, last_seen_at, expires_at | Configured lifetime |
| `sections` | id, slug (unique, fixed), name (unique), sort, notification_channel_id | Until deleted |
| `notification_channels` | id, name (unique), slack_channel, created_at | Until deleted |
| `sources` | id, name, url, section_id, enabled, user_agent, etag, last_modified, cursor_at, last_guids, last_fetch_at, last_status, consecutive_failures, retry_after | Permanent |
| `raw_items` | id, source_id, guid_hash, url, title, snippet, published_at, fetched_at, status, decision, reason, story_id, attempts | 48h |
| `stories` | id, status (`queued`/`publishing`/`published`), title, summary_md, why_it_matters, tldr, section_id, score, priority, entities (jsonb), search (tsvector), model, created_at, published_at, expires_at, slack_channel, slack_ts, comment_count | Until `expires_at`, or end of day if never published |
| `story_links` | story_id, source_id, url, title, is_primary, added_at | With the story |
| `story_updates` | id, story_id, note, url, created_at | With the story |
| `comments` | id, story_id, parent_id, user_id, body_md, points, created_at, edited_at, deleted | With the story |
| `comment_votes` | user_id, comment_id, value (±1), created_at | With the comment or user |
| `prompts` | id, section_id (null = global), version, body, author, created_at | Permanent (versioned); goes with its section |
| `settings` | key, value (jsonb), updated_by, updated_at | Permanent |
| `pacing_arrivals` | arrived_at, score | 4 weeks |
| `pacing_state` | next_slot_at (single row) | Permanent |
| `slack_outbox` | id, story_id, update_id (null for the alert, set for a reply), channel_id, slack_channel, status (`pending`/`sent`/`failed`), attempts, next_attempt_at, last_error, created_at, sent_at | Sent or failed + 7 days; with the story |
| `llm_providers` | id, name, type, base_url, api_key_enc, api_key_last4, enabled, created_at | Permanent |
| `llm_models` | id, provider_id, model_id, supports_json_schema, enabled | Permanent |
| `llm_calls` | id, stage, provider_id, model_id, input_tokens, output_tokens, cache_read_tokens, latency_ms, outcome, error, created_at | 30 days |
| `job_runs` | name, last_started_at, last_finished_at, last_error | Permanent |
| `audit_log` | id, actor_id, actor_label, action, target, details, created_at | 90 days |

## Notes

- Foreign keys to `users` cascade for sessions and votes. Two are set to null instead:
  - `comments.user_id`: a deleted user's comments are blanked first, so threads keep their shape;
  - `audit_log.actor_id`: the entry keeps the actor's name in `actor_label`.

  That way deleting a user, including the master during recovery, is a single `DELETE`.
- The master account's uniqueness is a partial unique index on `is_master`.
- `stories.search` has a GIN index for the dedup pre-filter.
