# commandcode2api

> Command Code API → OpenAI / Anthropic compatible endpoints, in Go.
> Single static binary, CGO-free, with an optional server-side key pool.

A Go rewrite of [MAXeaglet/commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy)
(here: the `hongkongkiwi/commandcode-proxy` fork), built by analyzing official CLI
network traffic to accurately replicate the Command Code API request protocol —
device fingerprint, lifecycle pre-requests, and the full `command-code@1.53.1`
wire dialect.

**Features**: OpenAI Chat Completions / **Responses API (`/v1/responses`)** + Anthropic
Messages API | Streaming & non-streaming | Tool calling (tool_use) | Multimodal image
input | Reasoning effort | Dynamic model list | Cache-hit metrics | Device fingerprint
disguise (per-key, deterministic, auto-refresh) | `x-api-key` auth | Client-disconnect
upstream abort | Zero-output → 429 auto-retry | Consecutive-timeout hints | Privacy-aware
logging | **Server-side multi-key pool** (rotation, sticky affinity, quota tracking,
admin REST) — everything the Node original does, plus the pool.

**Why Go**: ~15MB static binary (vs Node runtime), single-digit-MB steady-state RSS,
~2× request-body memory ceiling vs the reference's measured 5.5× amplification, no
event-loop backpressure surprises (TCP flow control is native).

## Quick Start

```bash
go build -o commandcode2api .
./commandcode2api                      # reads ./config.json, listens on :3050 (repo config)
```

The API key is passed per request via `Authorization` (or `x-api-key` for Anthropic
SDKs) — no need to store it in config. Key must start with `user_`:

```bash
curl http://127.0.0.1:3050/v1/chat/completions \
  -H "Authorization: Bearer user_xxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

Docker:

```bash
docker compose up -d            # listens on http://0.0.0.0:3050
```

macOS launchd (auto-start at login, keep-alive):

```bash
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/commandcode2api .
mkdir -p ~/.commandcode2api/data ~/.commandcode2api/logs
cp config.json ~/.commandcode2api/config.json   # set host to 127.0.0.1 for local use
# optional pool secrets in ~/.commandcode2api/.secrets.env:
#   export CC_VAULT_SECRET=... CC_ADMIN_TOKEN=...
# wrapper: ~/.commandcode2api/run.sh execs the binary with -config/-state-dir
# agent:   ~/Library/LaunchAgents/com.hongkongkiwi.commandcode2api.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hongkongkiwi.commandcode2api.plist
```

## Endpoints

| Endpoint | Protocol | Notes |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | stream + non-stream, tools, vision, `reasoning_content` |
| `POST /v1/messages` | Anthropic Messages | stream + non-stream, tool_use, thinking blocks |
| `POST /v1/responses` | OpenAI Responses (Codex) | stateless; `previous_response_id` → 400 |
| `GET /v1/models` | OpenAI | live provider catalog (5-min cache) + hardcoded fallback |
| `GET /health` | — | `OK` |
| `/admin/*` | JSON REST | key-pool management (`CC_ADMIN_TOKEN`) |

## Key pool (optional)

Pass-through behavior is unchanged with zero configuration. To run a pooled
gateway (client holds one `sk-` key; the proxy rotates real `user_` keys):

```bash
mkdir data
./commandcode2api -state-dir ./data \
  CC_VAULT_SECRET=... CC_ADMIN_TOKEN=...   # or export them
```

```bash
# add pooled keys (encrypted at rest when CC_VAULT_SECRET is set)
curl -X POST localhost:3050/admin/keys -H "Authorization: Bearer $CC_ADMIN_TOKEN" \
  -d '{"name":"acct1","key":"user_XXXX","priority":100}'

# mint a gateway key (plaintext shown once)
curl -X POST localhost:3050/admin/gateway-keys -H "Authorization: Bearer $CC_ADMIN_TOKEN" \
  -d '{"name":"my-gw","rpmLimit":60}'
```

Then call any `/v1/*` endpoint with `Authorization: Bearer sk-...`. Selection is
priority-tier round-robin with 1h sticky session affinity (preserves the upstream
prompt cache per conversation); 429/402/USAGE_EXCEEDED latch cooldowns and fail
over to the next key before any response bytes are written. Env: `CC_POOL_ONLY=1`
disables pass-through; `CC_ADMIN_TOKEN` gates `/admin/*`.

## Configuration

Drop-in compatible with commandcode-proxy: same `config.json` fields, same
environment variables, same defaults.

| Env | Default | Description |
|---|---|---|
| `PORT` / `HOST` | `3000` / `0.0.0.0` | Listen address (repo config.json ships `:3050`) |
| `CC_API_BASE` | `https://api.commandcode.ai` | Upstream base URL |
| `CC_UPSTREAM_PROXY` | unset | HTTP CONNECT proxy for upstream calls (fingerprint/lifecycle included — one account, one IP) |
| `PROJECT_SLUG` | `cc-proxy` | `x-project-slug` fallback |
| `CC_USE_PROVIDER_MODELS` | `true` | Dynamic model list |
| `CMD_ZDR` | off | `1` → ZDR-only routing (`x-cmd-zdr: 1`) |
| `CC_CLI_MODE` | `agent` | Envelope `mode` |
| `CC_CLI_SESSION_MODE` | `interactive` | Lifecycle metadata `mode` |
| `CC_FINGERPRINT_SALT` | empty | Rotate the whole fleet's device identity |
| `CC_DEVICE_PROJECT_DIR` | built-in `C:\Users\dev\projects\app` | Faked project dir (feeds fingerprint + `x-project-slug`) |
| `CC_EMPTY_SYSTEM_PLACEHOLDER` | `true` | Space placeholder when no system prompt (blocks upstream's ~7.5K-token default, issue #17) |
| `CC_MAX_BODY_MB` | `100` | Request body cap → `413` (connection drained, not reset) |
| `CC_STREAM_IDLE_MS` | `30000` | Streaming idle watchdog (per-chunk, resets on data) → 429 + retry hint |
| `CC_NONSTREAM_IDLE_MS` | `90000` | Non-streaming idle watchdog |
| `CC_MAX_INFLIGHT` | `0` (unlimited) | Global concurrency cap → `503 server_busy` (`/health` exempt) |
| `CC_CLIENT_DRAIN_TIMEOUT_MS` | unset | (Node-era knob; Go's TCP backpressure handles this natively — reserved) |
| `CC_KEEPALIVE_TIMEOUT_MS` | `65000` | Server idle timeout; keep reverse-proxy keepalive *below* this |
| `CC_STATE_DIR` / `-state-dir` | unset | Enables the key pool (`<dir>/pool.db`) |
| `CC_VAULT_SECRET` | unset | AES-256-GCM seed for pooled keys at rest |
| `CC_ADMIN_TOKEN` | unset | Bearer token for `/admin/*` |
| `CC_POOL_ONLY` | off | `1` disables `user_*` pass-through |

## Error semantics (ported 1:1)

| Upstream | Downstream |
|---|---|
| `400`/`422` | `400 invalid_request_error` |
| `401` / `403` | `401 authentication_error` |
| `402` | `429 rate_limit_error` (payment failure ≡ rate limit) |
| `429` | `429 rate_limit_error` + `retry_after: 30` |
| `500`/`502` | `502 upstream_error`; `503` → `503 temporarily_unavailable`; else `502` |

In-stream `error` events honor the upstream `statusCode` (the CLI's
`"<NNN>"`-in-message prefix wins when present). Truncation is never faked:
`max_output_tokens`/`model_context_window_exceeded` → `length`/`max_tokens`/
`incomplete`; `pause_turn` passes through on Anthropic; a stream with no finish
event → retryable `502 upstream_error` — never a fabricated `[DONE]` or
`message_stop`. Zero-output responses → `429` (anti false billing). Anthropic
`input_tokens` counts only the non-cached portion (issue #25 double-count fix).

## Integration examples

<details><summary>Python (OpenAI SDK)</summary>

```python
from openai import OpenAI
client = OpenAI(api_key="user_xxxxxxxxx", base_url="http://127.0.0.1:3050/v1")
stream = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "hello"}], stream=True)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="")
```
</details>

<details><summary>Anthropic SDK</summary>

```python
import anthropic
client = anthropic.Anthropic(api_key="user_xxxxxxxxx", base_url="http://127.0.0.1:3050")
print(client.messages.create(model="claude-sonnet-4-6", max_tokens=1000,
      messages=[{"role":"user","content":"hello"}]).content[0].text)
```
</details>

<details><summary>Codex / Responses</summary>

```bash
curl http://127.0.0.1:3050/v1/responses \
  -H "Authorization: Bearer user_xxxxxxxxx" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}'
```
</details>

## Development

```bash
go test ./...                        # 41 tests: contract suites ported from the Node repo
./scripts/smoke-live.sh              # live smoke (CC_LIVE_KEY=user_... required)
```

The mock Command Code upstream in `test/testutil` replays canned NDJSON, so the
whole behavioral contract runs offline. Fingerprint output is golden-vector tested
against the Node reference implementation.

## Disclaimer

For educational and research purposes only. Unofficial — not affiliated with
Command Code. You assume all responsibility and must comply with the Command Code
Terms of Service. The API key is forwarded per request and never logged or stored
outside the (encrypted) pool database. Keep usage patterns consistent with normal
CLI usage; extremely high concurrency may trigger upstream risk controls.

## License

MIT — see [LICENSE](LICENSE). Ported from
[MAXeaglet/commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy).

中文文档见 [README_zh.md](README_zh.md)。
