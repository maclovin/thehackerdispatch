# Getting started

After the first deploy, the system runs but does nothing useful until an admin configures it. Do these steps in order.

1. **Sign in as `admin`** with the master password. Read it from inside the container:

   ```sh
   docker compose exec app /thehackerdispatch master-password                                # local
   kubectl -n thehackerdispatch exec deploy/thehackerdispatch -- /thehackerdispatch master-password   # Kubernetes
   ```

   Keep the password: the file is deleted on the first login with it. If it's lost, see [Lost master password](deployment.md#lost-master-password).

2. **Admin panel → settings:** set the company email domains first. No other account can sign in until they are set.

3. **Admin panel → sources:** add RSS/Atom feeds.
   - "Test feed" shows what a feed returns before you save it.
   - "Import OPML" adds many at once. [`sources.opml`](sources.opml), next to these docs, is a ready-made catalog of 39 high-signal security feeds: CERTs and government advisories, vendor PSIRTs, threat intel and vulnerability research teams, and a few news sites. They all go to the `security` section; disable what you don't need after importing.

4. **Admin panel → sections and prompt:** adjust the sections, and write the global prompt (who reads the board, what matters, how to write) and, where useful, a prompt per section. A fixed part that can't be edited always comes first, so a prompt can't break the system.

5. **Admin panel → llm:**
   - add a provider (Anthropic, OpenAI, or any OpenAI-compatible API) with its API key. For Gemini, pick `openai-compatible` with the base URL `https://generativelanguage.googleapis.com/v1beta/openai/`;
   - add a model and press "test";
   - choose the curation model (and, optionally, a writing model and fallbacks).

   Curation stays paused until a model is set.

6. **Admin panel → settings:** poll interval, how long content lives, the daily cap, the minimum score, and the minimum time between stories.

7. **Admin panel → channels** (optional): add the Slack channels that get alerts and press "send a test". Then pick a channel on each section's page. This needs `SLACK_BOT_TOKEN`: a Slack app's bot token with the `chat:write` scope, with the app invited to each channel.

8. **Admin panel → decisions:** see what the model decided for each feed item, and why. Use it to tune the prompts.
