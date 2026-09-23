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

Non-chat modes ignore both flags.

## API keys

Use the `os.environ/<NAME>` indirection so secrets never sit in the YAML:

```yaml
api_key: "os.environ/CHATGPT_API_KEY"
```

The proxy resolves it once at config load. The current providers and their env vars:

| Provider        | `litellm_params.model` prefix | Env var              |
| --------------- | ----------------------------- | -------------------- |
| OpenAI          | _(none — bare name)_          | `CHATGPT_API_KEY`    |
| Anthropic       | `anthropic/`                  | `CLAUDE_API_KEY`     |
| Google          | `gemini/`                     | `GOOGLE_API_KEY`     |
| OpenRouter      | `openrouter/`                 | `OPENROUTER_API_KEY` |
| Fireworks       | `fireworks_ai/`               | `FIREWORK_KEY`       |
| Together        | `together_ai/`                | `TOGETHER_API_KEY`   |
| IO Intelligence | `openai/` + `api_base`        | `IONET_API_KEY`      |

Set these in `server/.env` (loaded via `godotenv` at startup) or in the shell before running the server.

IO Intelligence (io.net) is an OpenAI-compatible endpoint, so entries use the `openai/` prefix plus an explicit `api_base` — the model id after the prefix is the upstream `org/name` id (e.g. `openai/meta-llama/Llama-3.3-70B-Instruct`):

```yaml
- model_name: "llama-3.3-70b"
  litellm_params:
    model: "openai/meta-llama/Llama-3.3-70B-Instruct"
    api_key: "os.environ/IONET_API_KEY"
    api_base: "https://api.intelligence.io.solutions/api/v1"
  model_info:
    mode: "chat"
    supports_vision: false
    supports_function_calling: true
```

Access tiers: io.net's catalog flags some models as `higher_tier_required` — at the time of writing `zai-org/GLM-5.3` and `deepseek-ai/DeepSeek-R1-0528` need an io.net access tier above basic, while `meta-llama/Llama-3.3-70B-Instruct` works on the basic tier. A key without the required tier gets an authentication/permission error from io.net itself.

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
