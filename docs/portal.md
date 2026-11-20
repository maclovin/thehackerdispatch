# Portal and admin panel (`web`)

Server-rendered HTML (Go templates) with htmx for votes and comments. No SPA.

## Look and feel

Between Ableton and Craigslist:

- from Craigslist, the old-web text: system fonts, blue underlined links that turn purple once visited, dense lists, no images or cards;
- from Ableton, the layout: a strict grid (a sidebar of sections next to the list), flat color blocks, thick black rules, straight edges, rectangular buttons that invert on hover, and heavy sans headings.

Every page's footer states the TTL ("everything here disappears 7 days after it's published").

### Themes

Light and dark. By default the page follows the OS (`prefers-color-scheme`); each user can pick `system`, `light`, or `dark` in their profile.

### Branding (white-label)

Admins set three things; everything else (backgrounds, text, borders, fonts) is fixed:

- the **display name**, shown in the header, page titles, the login page, and Slack links (default "The Hacker Dispatch");
- one **primary color**, for the header bar and accents, in both themes;
- the **link colors** (link and visited), with separate values for light and dark.

They are injected as CSS custom properties in the base template; there is no build step.

## Pages

| Path | Page |
|---|---|
| `/` | The board |
| `/s/{id}` | Story and thread |
| `/admin/...` | Admin panel |

## Board

- Always in publication order, newest first. A story never moves: updates show on its page and Slack thread only.
- Posts have no votes and there is no algorithmic ranking. Critical posts carry a badge but keep their place.
- **Section filter:** a sidebar lists all sections; people tick the ones they want (e.g. `/?filter=1&sections=security&sections=tech`), and the choice is saved as their default.

## Story page

Title, summary, and why it matters; all source links (opened in a new tab); updates; an expiry countdown ("expires in 3d"); and the threaded comments.

## Comments and votes

- Each level of a thread is ranked by net points (up − down); ties go to the older comment.
- A sanitized Markdown subset (links, code, emphasis).
- Authors can edit for 15 minutes after posting.
- A comment deleted while it has replies becomes `[deleted]`.
- No moderation queue; admins can delete anything.
- **Votes** exist only on comments: one per person per comment, toggleable, never on your own comment.

## Admin panel

- **Sources:** list with health (last fetch, status, wait after a `429`, disabled after 10 failures, items in the last 48h); create, edit, disable, change section, override the User-Agent; **test** a feed; **import OPML** (feeds in a group named like a section go to that section, the rest to a section picked on import).
- **Sections:** create, rename, reorder, and delete (only when no source or story uses it). Each has an optional prompt and notification channel. The slug is fixed at creation, since it appears in links and saved filters.
- **Notification channels:** add, edit, delete, and send a test. The list shows each channel's sections and how its latest message went.
- **Prompts:** one global prompt plus an optional prompt per section, used for both curation and writing. Version history with restore; saving the same text doesn't create a version. The fixed contracts are shown read-only.
- **LLM:** providers (add, paste the API key, test), models, and per stage the primary model, fallbacks, and effort. See [LLM integration](llm.md).
- **Settings:** poll interval, TTL, daily cap, minimum gap, minimum score; allowed email domains, session lifetime, failed-login limits; branding. See [Configuration](configuration.md#runtime-settings-admin-panel).
- **Stories:** the board's stories, newest first. Deleting one removes its whole thread. Deletion lives only here, not on story pages.
- **Recent decisions:** items from the last 48h with the LLM's decision and reason, for tuning the prompts.
- **Users:** list (nickname, name, email, login mode, last access; the only place emails appear); create local accounts, switch login mode, generate a temporary password, promote or demote, ban or delete; see and kill sessions.
