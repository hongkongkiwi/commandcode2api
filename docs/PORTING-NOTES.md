# Porting notes: proxy.mjs → Go

Mapping from the reference implementation (`refs/commandcode-proxy/proxy.mjs`,
the normative spec) into this repository. When re-aligning after upstream CLI
drift, find the behavior here first, then update the Go file listed.

| proxy.mjs section | Go location |
|---|---|
| `loadConfig` | `internal/config/config.go` (fields + `applyEnv`; knobs as `EnvInt` accessors) |
| Device fingerprint (`fpDigest`, `fpPickIndex`, `fingerprintHash`, `generateFingerprint`, `DEVICE_PROFILE`) | `internal/cc/fingerprint.go` — golden-vector tested (`internal/cc/fingerprint_test.go` + `testdata/fingerprint_golden.json`, generated from the Node code) |
| `slugifyProjectPath` | `internal/cc/fingerprint.go` `SlugifyProjectPath` |
| Protocol drift check | `internal/cc/client.go` `StartDriftCheck` (warn-only, version pinned in `ids.go` `CCProtocolVersion`) |
| Sessions (`sessionStore`, `getSessionId`, per-key state) | `internal/cc/session.go` |
| `ensureInitialized` (fingerprint + lifecycle, 8h+2h jitter) | `internal/cc/client.go` `EnsureInitialized` |
| `buildCcRequest` + `toWire*` + threadId ordering | `internal/cc/envelope.go` `BuildEnvelope` |
| `createSseTranslator` (CC NDJSON → OpenAI SSE) | `internal/translate/openai.go` `ChatTranslator` |
| non-stream chat accumulation | `internal/translate/openai.go` `ChatAggregate` |
| `mapFinishReason` / `incompleteUpstreamDetail` / `normalizeUsage` / `anthropicInputTokens` | `internal/cc/events.go` |
| `CC_STATUS_MAP` / `mapCcError` / `mapCcEventError` / `incompleteUpstreamError` | `internal/cc/errors.go` |
| `readBody` (413 + drain) | `internal/server/helpers.go` `readBody` |
| `createIdleWatchdog` (per-chunk idle) | `internal/translate/pump.go` `PumpLines` |
| `waitDrain` / backpressure | native TCP backpressure (synchronous writes); `CC_CLIENT_DRAIN_TIMEOUT_MS` reserved |
| `forwardToCC` (headers, threadId reorder) | `internal/cc/client.go` `GenerateHeaders`/`Generate` |
| upstream HTTP proxy (`proxyFetch` CONNECT) | `internal/cc/client.go` — Go's `http.Transport` + `http.ProxyURL` does CONNECT with target-hostname cert validation (same guarantee, stdlib) |
| `handleChatCompletions` (delayed 200, keepalives, terminal precedence) | `internal/server/chat.go` |
| Anthropic conversion + SSE translator + `fakeThinkingSignature` | `internal/translate/anthropic.go` |
| `handleMessages` (buffered message_start, ping heartbeat*) | `internal/server/messages.go` (*ping heartbeat during long silences not yet ported — SSE comment keepalive only; add if clients time out during long thinking) |
| Responses conversion + SSE + objects | `internal/translate/responses.go`, `responses_sse.go` |
| `handleResponses` | `internal/server/responses.go` |
| `fetchModels` / `MODELS` | `internal/server/models.go` |
| `handleModels`, server, CORS, inflight | `internal/server/models_handler.go`, `mux.go` |

## Known deviations (deliberate)

1. **Memory**: request body is read once into one buffer and re-marshaled once
   (≈2× vs the reference's measured 5.5×). The 100MB default cap still applies.
2. **SSE keepalives**: chat emits a keepalive comment only while a read
   produces no visible events; the Anthropic 5s ping-timer is not ported yet.
3. **`CC_CLIENT_DRAIN_TIMEOUT_MS`** is accepted but inert: Go writes block on
   the socket, so a stalled client stalls the pump and TCP eventually kills
   the connection; there is no writer queue to leak.
4. **Key pool** (new, absent in the reference): `internal/pool`, `internal/store`,
   `internal/server/poolgate.go`/`admin.go`. Pass-through path is untouched.

## Test corpus

- `test/stream_end_test.go` — port of `test/stream-end.test.mjs` (issue #38 contracts)
- `test/connection_lifecycle_test.go` — port of `test/connection-lifecycle.test.mjs`
  (statusCode propagation, silent-event list, idle-timeout clean end)
- `test/pool_test.go` — pool behavior (dual auth, rotation, stickiness, admin)
- `internal/cc/envelope_test.go` — envelope wire-shape contracts
- `internal/cc/fingerprint_test.go` — byte-exact fingerprint vectors

When the upstream CLI changes: diff `refs/commandcode-proxy/proxy.mjs` against
its new release, update the pinned `CCProtocolVersion` **only** if the wire
dialect was re-verified, and extend the mock corpus in `testutil` before
touching Go code.
