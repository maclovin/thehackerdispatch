# Configuration

Only infrastructure and secrets come from environment variables. Everything else (sources, sections, prompts, LLM providers, schedules, limits, branding, sign-in rules) is configured at runtime in the admin panel and stored in the database.

## Environment variables

| Variable | Required | Kubernetes | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | `secrets.env` | PostgreSQL connection URL |
| `BASE_URL` | yes | `config.env` | Public URL of the portal, e.g. `https://news.example.com` |
| `SECRETS_KEY` | yes | `secrets.env` | 32 random bytes, base64 (`openssl rand -base64 32`). An AES-256 key that encrypts the LLM API keys saved in the database and the short-lived SSO login cookie |
| `SLACK_BOT_TOKEN` | no | `secrets.env` | Slack bot token (`xoxb-…`, scope `chat:write`) for the alerts |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID` | no | `config.env` | SSO through OpenID Connect: the identity provider's issuer URL (with Keycloak, the realm URL) and the app's client ID. The client's redirect URI is `<BASE_URL>/auth/callback` |
| `OIDC_CLIENT_SECRET` | no | `secrets.env` | The client's secret. Set all three `OIDC_*` variables or none |
| `LOG_LEVEL` | no | `config.env` | `debug`, `info` (default), `warn`, or `error` |
| `HTTP_ADDR` | no | (leave unset) | Listen address (default `:8080`) |
| `MASTER_PASSWORD_FILE` | no | (leave unset) | Where the bootstrap writes the master password (default `/tmp/thehackerdispatch/master-password`) |

The "Kubernetes" column says which overlay file holds the variable; see [Deployment](deployment.md#kubernetes).

## `SECRETS_KEY`

Keep a copy somewhere safe. If it changes, the LLM API keys saved in the panel can't be decrypted and must be entered again.

## Optional dependencies

| Dependency | Without it |
|---|---|
| LLM provider | Ingestion runs; curation and publishing pause, with a notice in the panel |
| Slack (`SLACK_BOT_TOKEN`) | No alerts; the portal works normally |
| Identity provider (`OIDC_*`) | Local accounts only |

## Runtime settings (admin panel)

| Setting | Options or default |
|---|---|
| Poll interval | 30 min, 1h, 2h, 3h, 5h, or 8h |
| Content lifetime (TTL) | 1, 7, 15, 30, or 90 days, or forever |
| Daily cap | 10 posts |
| Minimum gap between posts | 30 min |
| Minimum score | 40 |
| Allowed email domains | none (required before anyone but the master can sign in) |
| Session lifetime | 12h |
| Failed logins before a wait / wait time | 5 / 15 min |
| Branding | display name (default "The Hacker Dispatch"), primary color, link colors per theme |
