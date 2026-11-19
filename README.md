# cpa-mistral-vibe

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that adds
**Mistral** as a provider through the **Vibe browser sign-in** (`console.mistral.ai`),
so requests draw on your subscription's **Vibe Code allowance** instead of a
pay-as-you-go API key.

It does three things:

- **OAuth login** — a browser PKCE sign-in (the same flow the Mistral Vibe CLI
  uses) stored as a CPA credential, so you don't paste a static key. Multiple
  Mistral accounts can be added.
- **Quota card** — the monthly **Vibe Code** and **API** allowances
  (used / total, % remaining, reset date) in the native *Quota Management* page,
  read live from Mistral's billing endpoint.
- **Native model discovery** — the chat-capable models are listed dynamically
  from Mistral's `/v1/models` (DeepSeek, GLM/Z.ai, Mistral, Magistral, Ministral,
  Codestral, Voxtral, …). No hard-coded catalog.

Inference itself runs on CPA's built-in OpenAI-compatible executor: the plugin
stores `base_url` and `api_key` on the credential's attributes, and the host
handles translation, streaming and request logging.

## Install

From the CLIProxyAPI Management Center, install from this repository's release
(`install.type: github-release`), then enable it:

```yaml
plugins:
  enabled: true
  configs:
    cpa-mistral-vibe:
      enabled: true
```

Restart or reload CPA, open **Quota Management → Plugin**, and use the Mistral
**OAuth login** to sign in. A `mistral-<id>.json` credential is created and its
Vibe Code / API allowance appears as a card.

### Build manually

Requires Go 1.26 with cgo (the plugin is a `c-shared` library).

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags="-s -w" \
  -o cpa-mistral-vibe.so ./cmd/cpa-mistral-vibe
```

Place `cpa-mistral-vibe.so` under `<plugins.dir>/linux/<arch>/` and add the
config block above. Tagged releases (`vX.Y.Z`) build `linux/amd64` and
`linux/arm64` automatically.

## Configuration

| Field | Description |
|---|---|
| `api-key` | Optional. A `mstrl_…` key to use directly. Leave empty to use the signed-in credential (recommended). |
| `config-path` | Optional. Path to the CPA config file the plugin may read a key from. Default `/app/config.yaml`. |
| `strip-excess-images` | Optional, default `false`. Cap the images sent to Mistral per request (see [Image cap](#image-cap)). |
| `max-images` | Optional, default `20`. The cap used by `strip-excess-images`. |

The provider key is `mistral`; credentials are `mistral-*.json` auth files.

## Normalization

Mistral's chat API has a few quirks that OpenAI-shaped clients (e.g. Codex) trip
on. **This plugin handles all of them automatically — no config, no extra
plugin.** Everything below is scoped to this plugin's own Mistral models, so
other providers (codex, claude, …) are never touched.

**Request** fixes (via the `request_interceptor` and `request_normalizer`
capabilities, so they apply to both Chat Completions and Responses-API clients
such as Codex):

- `developer` role → `system` (Mistral rejects `developer`).
- `reasoning_effort: medium` / `reasoning.effort: medium` → `high` (most
  Mistral models reject `medium`).
- Drop `reasoning_content` from assistant history (Mistral 422s on it).

**Response** fix (via the `response_before_translator` capability): when a model
reasons, Mistral returns `content` as a list of thinking/text chunks; the plugin
flattens it into a `content` string plus a `reasoning_content` string (streaming
and non-streaming) and strips Mistral's non-standard `response_error`. This logic
is ported from
[`cpa-mistral-normalizer`](https://github.com/Batuhan4/cpa-mistral-normalizer)
(MIT, © batuhan4); you do **not** need to install that plugin.

## Image cap

Mistral rejects a request that carries more than 20 images in total, counted
across the whole conversation, with `400` code `3051` ("Total number of images
exceeds the maximum allowed of 20"). Clients resend their full history every
turn, so once a conversation passes 20 images every later turn fails, even a
text-only one.

Set `strip-excess-images: true` to have the plugin replace the oldest images
with a short text note before the request reaches Mistral:

```yaml
plugins:
  configs:
    cpa-mistral-vibe:
      enabled: true
      strip-excess-images: true
      max-images: 20
```

- Only the request sent to Mistral changes. The client keeps every image in
  its own history; each request is counted again from scratch, and turning
  the option off sends everything again on the next request.
- Images are dropped in blocks of half the limit (10 at the default), so a
  request keeps between 11 and 20 images and the start of the prompt does not
  shift on every new image.
- A dropped image becomes a text part in the same message, so a tool result
  that held only an image keeps its place in the conversation.
- It applies to every client of the CPA instance that uses this plugin's
  Mistral models; there is no per-client switch.

## Caveats

- Quota reads use Mistral's **internal** `/v1/billing/budget` endpoint, which is
  not a published API and can change without notice. The `$150` / `$15` figures
  are plan constants; the API returns only the used percentage.
- The allowance resets on the **1st of each month, 00:00 UTC**. The Quota card
  renders that in your browser's locale and timezone.
- A fresh sign-in creates a credential keyed by the key's hash (no stable
  account id is available), so re-logging in the same account adds a new entry —
  delete the old one if it duplicates.
- Sharing a Mistral subscription through Vibe for programmatic use may be subject
  to Mistral's terms; use your own account.

## License

MIT
