# CodeBuddy / WorkBuddy API Service

Local OpenAI-compatible gateway backed by CodeBuddy, CodeBuddy CN and WorkBuddy
subscription accounts. It is the CodeBuddy-family counterpart of the Codex API
Service described in `CODEX_API_SERVICE_HANDOFF.md`.

## Architecture

```text
Third-party OpenAI-compatible client
  -> http://127.0.0.1:<port>/v1            (client API key)
  -> cockpit-cliproxy sidecar              (CodeBuddy upstream adapter)
  -> https://<gateway>/v2/chat/completions (account JWT)
  -> Copilot gateway (APISIX)
```

Cockpit remains the token authority: `codebuddy_local_access.rs` resolves the
selected accounts, refreshes a token that is close to expiry, and materializes
`codebuddyUpstreams` into the generated manifest. The sidecar only refreshes
opportunistically on a 401 and reports the rotated pair back through a
`codebuddy_token_refreshed` event, which Rust writes back into the account store.

## Upstream protocol (verified live)

Verified on 2026-09-10 against `copilot.tencent.com` with a real account.

| Item | Value |
| --- | --- |
| Chat endpoint | `POST {baseUrl}/v2/chat/completions` |
| Auth | `Authorization: Bearer <JWT>` (Keycloak RS256, ~1 year) |
| Content type | `application/json;charset=UTF-8` |
| User agent | Browser UA; the gateway rejects unknown clients on some routes |
| `X-Agent-Intent` | Optional (verified working without it); `craft` by default |
| gzip request body | Not required |
| Streaming | **Required** — `stream:false` returns `code11101 Non-stream chat request is currently not supported` |
| Response shape | OpenAI `chat.completion.chunk` SSE + `data: [DONE]` |
| Token refresh | `POST {baseUrl}/v2/plugin/auth/token/refresh` with `X-Refresh-Token` + `X-Auth-Refresh-Source: plugin` |

Gateway hosts per platform:

| Platform | Gateway | Notes |
| --- | --- | --- |
| WorkBuddy | `https://copilot.tencent.com` | verified |
| CodeBuddy CN | `https://copilot.tencent.com` | verified (also accepts `www.codebuddy.cn`) |
| CodeBuddy | `https://www.codebuddy.ai` | not verifiable without an `.ai` account |

Model catalog: `GET /v3/config` rejects requests that are not from an IDE client
(`code 12403 check ua, get coding copilot version error`), and there is no public
`/v2/models`. The catalog is therefore a curated preset, trimmed per upstream.
Live-verified available models on a WorkBuddy account: `auto` (resolved to
`deepseek-v4.1-flash`), `glm-5.1`, `glm-5.0-turbo`, `kimi-k2.6`,
`deepseek-v3-2-volc`, `deepseek-r1-0528-lkeap`, `minimax-m2.7`, `hunyuan-chat`,
`hy3-preview`. `glm-4.7` was present in the reverse-engineering notes but is not
entitled on that account (`code 11102 model ... service info not found`).

`GET /v2/plugin/accounts` is a cheap authenticated probe that does not spend
credit.

## Adapter behaviour (`sidecars/cockpit-cliproxy/codebuddy_gateway.go`)

Request rewrite (`codebuddyRequestBody`):

- forces `stream: true`;
- drops `stream_options`, `logprobs`, `top_logprobs`, and `n > 1`;
- maps `max_completion_tokens` to `max_tokens`;
- keeps `messages`, `tools`, `tool_choice`, `temperature`, `top_p`.

Response rewrite (`sanitizeCodebuddyChunk`):

- rewrites `model` back to the client-requested id (the upstream returns the
  resolved endpoint id);
- removes `extra_fields`, empty `refusal`, empty `function_call`, and
  gateway-only usage keys (`credit`, `prompt_cache_*`, `cache_*`, `cached_tokens`);
- drops `reasoning_content` unless the collection enables it;
- passes `tool_calls` fragments through unchanged (already OpenAI-shaped).

Non-streaming clients get a synthesized `chat.completion` object aggregated from
the upstream stream, including merged `tool_calls` by index.

Account selection is round-robin (or random) across the scoped upstreams, with
fall-through to the next account when the upstream rejects the request before any
byte was written to the client.

## HTTP surface

| Method | Path | Behaviour |
| --- | --- | --- |
| GET | `/v1/models` | Union of the scoped upstream catalogs, `owned_by: "codebuddy"` |
| POST | `/v1/chat/completions` | Proxied (streaming and synthesized non-streaming) |
| Anything else | | `invalid_request` — CodeBuddy has no Responses/Anthropic/Gemini surface |

## Persistence

- `codebuddy_local_access.json`: collection (enabled, port, api key, scope,
  routing strategy, selected accounts, model filter).
- `codebuddy_local_access_sidecar/config.json` + `manifest.json`: generated
  runtime files; the manifest contains live JWTs and must stay local.

## Tests

- `sidecars/cockpit-cliproxy/codebuddy_gateway_test.go`: header/body rewrite,
  stream sanitization, non-stream aggregation, tool-call merging, error mapping,
  account fall-through, model endpoint.
- `src-tauri/src/modules/codebuddy_local_access.rs` `#[cfg(test)]`: collection
  normalization, JWT expiry decoding, manifest requirements, bind host, catalog
  fallback.

## Known limitations

- CodeBuddy has no image generation, Responses, or realtime API, so those
  endpoints return an explicit error rather than silently degrading.
- The model catalog is a preset, not discovered from the upstream.
- `codebuddy_token_refreshed` write-back is implemented; a token rotated while
  Cockpit is closed is only re-materialized on the next service start.
