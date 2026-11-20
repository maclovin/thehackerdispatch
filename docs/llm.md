# LLM integration (`llm`)

The system isn't tied to any provider. Any LLM reachable with an API key works, as long as it can return JSON.

## Providers

Configured in the admin panel. Each has a name, a type, a base URL (only for `openai-compatible`), and an API key.

| Type | Covers | Implementation |
|---|---|---|
| `anthropic` | Claude | Official Anthropic SDK |
| `openai` | OpenAI models | Official OpenAI SDK |
| `openai-compatible` | Any API in OpenAI's format: Google Gemini, OpenRouter, Groq, DeepSeek, Mistral, Ollama, vLLM, etc. | Same adapter as `openai`, with its own base URL |

For Gemini, use `openai-compatible` with the base URL `https://generativelanguage.googleapis.com/v1beta/openai/`. (A dedicated Gemini SDK made the binary five times larger for no extra capability.)

API keys are stored encrypted with `SECRETS_KEY` and never go back to the screen: the panel shows only the last 4 characters and a "test" button.

## Models and stages

- **Models:** each registered model has its provider, its model id, and whether it supports JSON schema.
- **Stages:** curation and writing each have an ordered list of models. The first is the primary; the rest are fallbacks on error, timeout, or refusal. If writing has no models, it uses curation's.
- **Effort:** each stage has a generic level (low, medium, high), which the adapter maps to the provider's reasoning parameter, or ignores if the model has none.

## Single interface

The rest of the system knows one operation: "given the system prompt, the message, and a JSON schema, return the JSON". Each adapter handles:

- **Structured output:** native JSON schema when supported; otherwise the schema goes in the instructions and the JSON is extracted from the answer. The server validates the result either way.
- **Normalized result:** `ok`, `refusal`, `truncated`, or `error`, with token usage (including cache) and latency.
- **Prompt caching:** the order contract → sections → global prompt → section prompt → known stories → new items keeps a stable prefix. The adapter marks the cache where the provider requires it.

## Prompts

Every call starts with a fixed contract that can't be edited, so the system always works. Then comes the global prompt, then the section's prompt if it has one. Both are edited in the panel, and every save is a new version. Prompts are the same for every provider.

## Failures and refusals

- An error or refusal moves the call to the next model in the list.
- **Curation, all models failed:** the items stay pending for the next cycle. An explicit refusal discards the item with reason `refused`.
- **Writing, all models failed:** the story is published with the feed's title and snippet.
- **No provider configured** (the state on first startup): ingestion runs, but curation and publishing pause, with a notice in the panel.

Every call is logged in `llm_calls` with provider, model, tokens, latency, outcome, and error.
