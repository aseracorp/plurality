# LiteLLM Configuration Guide

`server/data/litellm_config.yaml` is the **single source of truth** for which models the Plurality server exposes and what each one can do. The Go server holds no hardcoded model list, no provider URLs, and no provider API keys — everything is driven from this file.

## How it fits together

```
┌──────────────┐  /v1/chat/completions   ┌──────────────────┐
│  Go server   │ ──────────────────────▶ │ litellm_proxy.py │ ──▶ OpenAI / Anthropic / Gemini /
│              │  /v1/images/generations │ (Python, port    │     Fireworks / Together / …
│  ModelReg.   │ ◀────────────────────── │  4000 by default)│
│  ←/v1/models │  /v1/audio/speech       │                  │
└──────────────┘  /v1/audio/transcriptions└──────────────────┘
```

- The Python proxy reads `litellm_config.yaml` at startup.
- The Go server fetches `GET /v1/models` once at boot, caches every entry's capabilities in `ModelRegistry`, and uses that to validate model names, gate tool calls, route image/audio, and populate the picker.
- Editing the YAML and restarting the proxy + server is enough to add, remove, or re-flag a model.

## Anatomy of an entry

```yaml
- model_name: "claude-sonnet-4-6"          # ← the name you reference everywhere else (presets, client, API)
  litellm_params:
    model: "anthropic/claude-sonnet-4-6"   # ← the upstream model. The provider prefix (anthropic/, gemini/,
                                           #    fireworks_ai/, together_ai/, openai is implicit) tells LiteLLM
                                           #    where to route the call.
    api_key: "os.environ/CLAUDE_API_KEY"   # ← the env var the proxy reads at startup. The key never leaves
                                           #    the proxy process.
  model_info:
    mode: "chat"                           # ← see "The `mode` field" below
    supports_vision: true                  # ← drives the picker's vision filter and SelectModel logic
    supports_function_calling: true        # ← gates whether `tools` are sent to the LLM
```

`model_name` is the *internal* handle. It's what the client sends, what presets reference, and what the registry keys on. Keep it short and free of provider prefixes (e.g. prefer `gemini-2.5-flash` over `Gemini/gemini-2.5-flash`).

## The `mode` field

`mode` tells the server what kind of model this is and which proxy endpoint will accept it. It is the single most important field.

| Mode                  | Endpoint hit by Go              | What it's for                                  |
| --------------------- | ------------------------------- | ---------------------------------------------- |
| `chat`                | `/v1/chat/completions`          | Text chat. Supports streaming, tools, vision.  |
| `image_generation`    | `/v1/images/generations`        | Text → image (FLUX, etc.).                     |
| `audio_speech`        | `/v1/audio/speech`              | Text-to-speech (Cartesia, etc.).               |
| `audio_transcription` | `/v1/audio/transcriptions`      | Speech-to-text (Whisper).                      |
| `embedding`           | `/v1/embeddings`                | Embeddings for conversation search. Hidden from the picker. |

Anything `chat` is sent through LiteLLM's native router and benefits from cost tracking, token counting fallbacks, and `drop_params: true`. The other modes are **passthrough** — the proxy reads `model_info.endpoint_url`, attaches the configured API key, and forwards the request body verbatim.

## Capability flags

Set these on `model_info`. They flow through `/v1/models` into the Go `ModelRegistry`.

| Flag                        | Default | Effect                                                                                  |
| --------------------------- | ------- | --------------------------------------------------------------------------------------- |
| `supports_vision`           | `false` | Allows the model to receive image attachments. Adds it to the picker's vision dropdown. |
| `supports_function_calling` | `false` | Sends `tools` and skill prompts to this model. Without it, the model gets pure chat.    |
| `headroom`                  | `false` | Route this model's chat completions through the in-container Headroom optimization proxy. See "Headroom" below. |

Non-chat modes ignore both flags.

## Headroom (context optimization proxy)

The Plurality container bundles the [Headroom](https://docs.headroomlabs.ai) optimization proxy as its own venv at `/app/headroom/headroom_venv`. The Go server spawns it at boot on `127.0.0.1:8787` in front of the **local** LiteLLM proxy (`127.0.0.1:4000`), and routes a model's `/v1/chat/completions` through it **only** when that model opts in:

```yaml
model_info:
  headroom: true
```

Every model without the flag (and every image/audio/embedding request) continues to talk to LiteLLM directly, byte-for-byte untouched — zero added latency, zero risk to non-opted models.

**Prompt caching is preserved.** Headroom runs in `HEADROOM_MODE=cache`, which freezes prior turns byte-identically and forwards `cache_prompt`/`n_keep`/`extra_body` verbatim to the upstream. The llama.cpp/strata/avalon KV cache therefore keeps hitting the exact same prefix — the proxy never reorders, drops, or rewrites the input on its own. (Token mode, which rewrites history for token savings, would break prefix caching — that is why the deployment pins cache mode in `startHeadroomProcess`.)

Operational notes:

- `HEADROOM_URL` env var points the server at an externally managed Headroom instead of spawning one (analogous to `LITELLM_URL`); `HEADROOM_URL` is required when `LITELLM_URL` is set (no local upstream to front).
- If Headroom is unhealthy, opted-in models transparently fall back to direct LiteLLM — `cache_prompt`/`n_keep` forwarding lives in the LiteLLM proxy, so caching survives even a Headroom outage.
- Headroom artifacts (Kompress/ModernBERT) persist under the data volume (`<DATA_DIR>/headroom`) so a recreate does not re-download them.

### Headroom wiring

```
┌──────────────┐  chat (headroom:true)   ┌───────────────┐        ┌──────────────────┐
│  Go server   │ ──────────────────────▶ │ headroom proxy│ ─────▶ │ litellm_proxy.py │ ──▶ upstream
│              │                         │ 127.0.0.1:8787│        │ 127.0.0.1:4000    │
│  ModelReg.   │ ◀────────────────────── │ mode=cache    │ ◀───── │ (always direct    │
└──────────────┘  non-opted → direct     └───────────────┘        │  for non-opted)   │
                                                                    └──────────────────┘
```

## Caveman (compression proxy)

The Plurality container bundles [Caveman](https://github.com/JuliusBrussee/caveman) (Apache-2.0) as a pinned static binary at `/app/caveman/caveman-proxy` (release `bin-v4.0.2`). The Go server spawns it at boot on `127.0.0.1:8789`.

Spawning is not routing. `caveman: true` turns caveman's **stones** on for a model, and the stones Plurality implements (`say_less`, `read_less`, `browse`) all run **in Go** — they need no request hop. The binary joins the request path only for a model that explicitly asks for it:

```yaml
model_info:
  caveman:
    enabled: true
    proxy: true     # opt INTO the plurality -> caveman -> LiteLLM hop
                    # (headroom, when enabled, takes the hop first)
```

Caveman makes the model "say less and read less": `say_less` shrinks what the agent answers, `read_less` compresses tool output / logs / verbose turns, `browse` compresses web pages. Those three are implemented natively in Go (`src/caveman`), applied at message-write time and on the outbound request — no proxy involved.

The binary is a base-URL-swap reverse proxy: when `caveman.proxy: true` is set, the Go server points that model's chat URL at `/compat/litellm/v1/chat/completions`. **Measured on this deployment, that hop saves 0 input tokens and costs ~10x latency**: provider `prompt_tokens` are identical through the proxy and direct LiteLLM (651 == 651, 951 == 951; 736 == 736 at 5 KB and 196842 == 196842 at 469 KB per PR #191) with zero compression plans in the proxy ledger. That is the documented contract, not a bug: compress mode needs a recovery tool registered by a middleware adapter, and an entry point that cannot register one records `recovery_unbound` and forwards content unchanged. Plurality's recovery tool (`caveman_retrieve`) is a Plurality tool backed by the conversation DB, so it is not that registration. Use `proxy: true` only for the stones Go does not implement (`auto`, `blocks`, `waste_fixes`).

Direct LiteLLM also keeps the backend's **native** KV prompt cache — measured `cached_tokens` 946 of 951 on a self-hosted llama.cpp backend — so removing the hop loses no caching. When Caveman and Headroom are both enabled, the ownership resolver wires exactly ONE content compressor per model (see below).

**Composing Caveman and Headroom on the same model.** Both may be enabled at once, and each takes sub-settings:

```yaml
model_info:
  headroom:
    enabled: true
    keep_last_turns: 0        # trim N last turns before optimizing (0 = keep all)
  caveman:
    enabled: true
    stones:                    # caveman's six "rocks"
      say_less: true           # agent answers short, code stays exact
      read_less: true          # compress logs / JSON / CSV / YAML / test output before the model reads them
      browse: true             # compress web pages / Playwright snapshots
      auto: false              # pick model + effort per ask (needs a Caveman account)
      blocks: false            # keep scripts the agent wrote and re-run them
      waste_fixes: false       # keep the prompt cache warm while you pause
  proxy: false               # put the caveman BINARY in the request path (see below)
```

Bare `headroom: true` / `caveman: true` still work and mean "enabled with defaults". For caveman the default is **`say_less` + `read_less` + `browse` on, `auto` + `blocks` + `waste_fixes` off** — the three on-by-default stones need nothing external and can only shrink payloads (`browse` is gain-guarded, so it ships raw rather than growing a tool result), while `auto` requires a Caveman account and swaps the model mid-conversation, `blocks` is pre-release and re-executes shell code, and `waste_fixes` holds the KV cache open past the end of a request. Turn any of them on by name.

The default lives in exactly two places and they must agree: `DEFAULT_CAVEMAN_STONES` in `litellm_proxy.py` and `defaultCavemanStones()` in `src/ai/model_registry.go`. The proxy and the Go registry parse the same config from opposite ends (the proxy normalizes the YAML and publishes `/v1/models`; the registry parses that, and also has to cope with older images that publish a bare `{"enabled": true}`), so if the two sets differ, which stones are live depends on which side normalized the value last. Naming a subset of `stones` fills the rest from the same default on both sides. There is a test on each side pinning the set down (`test_litellm_proxy.py`, `model_registry_parse_test.go`).

**Two independent decisions.** Routing (transport) and content ownership are decided separately, by different rules:

* **Transport** comes from the proxy flags alone. `headroom: true` puts headroom in the loop for that model, **always** — including models that also enable caveman. `caveman.proxy: true` puts the caveman binary in the loop, and yields to headroom when both are set.
* **Content ownership** picks a single input compressor, to keep any payload category from being compressed twice. This is a decision about *who compresses*, never about *which hop runs*.

- **Caveman owns content** when it is enabled with the `read_less` stone on (its structured-content compressor handles JSON/log/CSV/YAML/test output, which headroom would otherwise also compress). That compression runs **in Go**, at message-write time and on the outbound request, so it adds no hop of its own. A model with `headroom: true` and caveman `read_less` therefore runs `plurality -> [caveman stones in Go] -> Headroom -> LiteLLM`.
- **Headroom owns content** when enabled without caveman `read_less`. Requests route `plurality -> Headroom -> LiteLLM`.
- Enabling "both" therefore does not chain two content compressors — caveman `read_less` wins the content, headroom keeps its hop — and the non-overlapping features compose on the same model: caveman's stones shrink the content in Go, and headroom's cache/`keep_last_turns`/output-savings apply on the request that results. `headroom.keep_last_turns` rides whenever headroom is the hop, not only when headroom owns content.

Measured on this deployment, headroom's own input compression saves **0 tokens** (`34585 == 34585` on a realistic JSON+build-log+CSV payload, `1508 == 1508` across three unique payloads), so a caveman-owned model reaching headroom does not in practice double-compress. Its value on this box is the cache and the output side. If a future headroom begins compressing input, `CAVEMAN_GO_READLESS=0` hands input back to headroom and restores the strict no-double rule. **KV prompt-cache stability is preserved** on both paths: caveman's `PrefixStabilizer` keeps Go-compressed prefixes byte-stable, headroom's `cache` mode freezes prior turns, and both forward `cache_prompt`/`n_keep` verbatim.

Routing / fail-closed: an unhealthy proxy is skipped and the request degrades stepwise — headroom down → the caveman binary if that model opted into `proxy` → **direct LiteLLM**. A caveman-owned model with the default `proxy: false` has no caveman hop to fail at all, so prompt caching and routing survive any caveman outage. `x-headroom-keep-last-turns` is only sent on a request that is actually going to headroom.

Note that caveman's `/compat/headroom` mount is written into `caveman.yaml` at boot (and answers `200` when probed), but **no routing path uses it**: plurality talks to headroom directly, so a chain through caveman's mount is never built. The `plurality -> Caveman -> Headroom -> LiteLLM` chain that earlier docs described has never existed in the routing code. Stated here so the next reader does not have to rediscover it from source.

Operational notes:

- `CAVEMAN_URL` env var points the server at an externally managed Caveman instead of spawning one (analogous to `LITELLM_URL`/`HEADROOM_URL`); required when `LITELLM_URL` is set.
- Caveman's SQLite spend store and generated `caveman.yaml` live under the data volume (`<DATA_DIR>/caveman`), so a recreate does not reset stats.
- Cache-warming (`CAVEMAN_CACHE_WARM`) is off: Plurality already sends explicit `cache_prompt`/`n_keep` hints, so Caveman does not make extra `max_tokens=0` upstream calls.

### Caveman wiring

```
┌──────────────┐  chat (caveman:true)    ┌────────────────┐      ┌───────────────┐      ┌──────────────────┐
│  Go server   │ ──────────────────────▶ │ caveman-proxy  │ ───▶ │ headroom proxy│ ───▶ │ litellm_proxy.py │ ──▶ upstream
│              │                         │ 127.0.0.1:8789 │      │ 127.0.0.1:8787│      │ 127.0.0.1:4000    │
│  ModelReg.   │ ◀────────────────────── │ mode=compress  │ ◀─── │ mode=cache    │ ◀─── │ (direct for       │
└──────────────┘  non-opted → direct     └────────────────┘      └───────────────┘      │  non-opted)       │
                                                                                          └──────────────────┘
```

## API keys

Use the `os.environ/<NAME>` indirection so secrets never sit in the YAML:

```yaml
api_key: "os.environ/CHATGPT_API_KEY"
```

The proxy resolves it once at config load. The current providers and their env vars:

| Provider     | `litellm_params.model` prefix | Env var            |
| ------------ | ----------------------------- | ------------------ |
| OpenAI       | _(none — bare name)_          | `CHATGPT_API_KEY`  |
| Anthropic    | `anthropic/`                  | `CLAUDE_API_KEY`   |
| Google       | `gemini/`                     | `GOOGLE_API_KEY`   |
| Fireworks    | `fireworks_ai/`               | `FIREWORK_KEY`     |
| Together     | `together_ai/`                | `TOGETHER_API_KEY` |

Set these in `server/.env` (loaded via `godotenv` at startup) or in the shell before running the server.

## Image gen, TTS, and STT — passthrough mode

These three modes don't go through LiteLLM's native router (LiteLLM's audio coverage is patchy outside OpenAI). Instead the proxy forwards the request to a URL you specify. Tell it where with `model_info.endpoint_url`:

```yaml
- model_name: "black-forest-labs/FLUX.2-pro"
  litellm_params:
    model: "together_ai/black-forest-labs/FLUX.2-pro"
    api_key: "os.environ/TOGETHER_API_KEY"
  model_info:
    mode: "image_generation"
    endpoint_url: "https://api.together.xyz/v1/images/generations"
```

The proxy will:

1. Receive the request from Go on `/v1/images/generations`.
2. Look up the entry by `model_name`.
3. POST the request body verbatim to `endpoint_url` with `Authorization: Bearer <resolved api_key>`.
4. Stream / return the response unchanged.

The same pattern applies to `audio_speech` and `audio_transcription`. The Go side never sees the URL or the key.

## Worked examples

### A vanilla chat model

```yaml
- model_name: "gpt-4.1"
  litellm_params:
    model: "gpt-4.1"
    api_key: "os.environ/CHATGPT_API_KEY"
  model_info:
    mode: "chat"
    supports_vision: true
    supports_function_calling: true
```

### A chat model that doesn't support tools

```yaml
- model_name: "deepseek-r1-0528"
  litellm_params:
    model: "fireworks_ai/accounts/fireworks/models/deepseek-r1-0528"
    api_key: "os.environ/FIREWORK_KEY"
  model_info:
    mode: "chat"
    # no supports_function_calling → server won't send `tools` to this model
```

### Image generation

```yaml
- model_name: "black-forest-labs/FLUX.2-dev"
  litellm_params:
    model: "together_ai/black-forest-labs/FLUX.2-dev"
    api_key: "os.environ/TOGETHER_API_KEY"
  model_info:
    mode: "image_generation"
    endpoint_url: "https://api.together.xyz/v1/images/generations"
```

### Text-to-speech

```yaml
- model_name: "cartesia/sonic"
  litellm_params:
    model: "together_ai/cartesia/sonic"
    api_key: "os.environ/TOGETHER_API_KEY"
  model_info:
    mode: "audio_speech"
    endpoint_url: "https://api.together.ai/v1/audio/generations"
```

### Speech-to-text

```yaml
- model_name: "whisper-v3-turbo"
  litellm_params:
    model: "fireworks_ai/whisper-v3-turbo"
    api_key: "os.environ/FIREWORK_KEY"
  model_info:
    mode: "audio_transcription"
    endpoint_url: "https://audio-turbo.us-virginia-1.direct.fireworks.ai/v1/audio/transcriptions"
```

### Embedding (used internally for conversation search)

```yaml
- model_name: "text-embedding-3-small"
  litellm_params:
    model: "text-embedding-3-small"
    api_key: "os.environ/CHATGPT_API_KEY"
  model_info:
    mode: "embedding"
```

Embedding entries are filtered out of the picker — they're called only by `search/embeddings.go`.

## Adding a new model

1. Append an entry under `model_list:` in `server/data/litellm_config.yaml`.
2. Set the `os.environ/...` API key reference (and export the env var if it's a new provider).
3. Set `model_info.mode` and the relevant capability flags.
4. For image / audio modes, also set `model_info.endpoint_url`.
5. Restart the server (`./server/build/Plurality` — restarting Go restarts the embedded litellm proxy). At runtime the file is read from `<exeDir>/data/litellm_config.yaml`; in dev that's `server/build/data/litellm_config.yaml`. `./build.sh` seeds it from `server/data/litellm_config.yaml` only when the build copy is missing (it never clobbers your edits).
6. Confirm the model appears: `curl http://127.0.0.1:4000/v1/models | jq` — it should show with the right `mode` and flags.

The Flutter picker will pick it up on the next reload of `/models`.

## Removing or re-flagging a model

- **Hide a model from users**: delete its entry. Old conversations referencing it will fail validation, so either migrate the data first or accept the dropped state.
- **Stop sending tools to a model**: remove `supports_function_calling: true` from its `model_info`. Restart the server. The `Models.IsActionModel(name)` gate immediately reports false.
- **Mark a model as vision-capable retroactively**: add `supports_vision: true`. The vision-pick logic in `SelectModel` will start routing image-bearing requests to it.

## Top-level settings

```yaml
general_settings:
  drop_params: true

litellm_settings:
  drop_params: true
```

`drop_params: true` tells LiteLLM to silently strip parameters that a given provider doesn't understand (e.g. `temperature` on a model that ignores it) instead of erroring. Leave this on unless you're debugging a specific param-mismatch issue.

## Gotchas

- **Restart required.** The proxy reads `litellm_config.yaml` once at startup. Edits don't hot-reload.
- **`model_name` must be unique.** Duplicate keys silently win/lose in YAML order — the LiteLLM router will use whichever entry is last.
- **Legacy prefixes.** The Go server used to prefix names with `ChatGPT/`, `Claude/`, `Gemini/`. A small shim in `API_index.go` strips those from incoming requests so old conversations keep working. New entries should always use bare names.
- **`endpoint_url` is required for non-chat passthrough modes.** Without it the proxy returns `400 Model … has no endpoint_url in model_info`.
- **Keys live in the proxy's process env, not Go's.** If you set `TOGETHER_API_KEY` only in the Go service's environment and not where the proxy is launched, image/audio calls will 401.

## Related code

- `server/data/litellm_config.yaml` — the file this doc describes.
- `server/litellm_proxy.py` — Python proxy: reads the YAML, exposes `/v1/models`, `/v1/chat/completions`, `/v1/embeddings`, and the three passthrough endpoints.
- `server/src/ai/model_registry.go` — Go-side `ModelRegistry` populated from `/v1/models` at boot.
- `server/src/ai/API_models.go` — projects the registry into the client-facing `/models` response (presets, function bundles, capability flags).
- `server/src/ai/litellm.go` — supervises the Python proxy subprocess.
