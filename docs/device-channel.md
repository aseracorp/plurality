# Device channel: device tools in background runs

Problem: device tools (`shell_client__exec`, `filesystem_client__fs_read|fs_write`,
client-side MCP) are advertised only by an app that streams THAT conversation
(`ChatPayload.ClientSideTools`). Background runs — cron, webhooks
(`jobs.RunPrompt`), sub-agents (`jobs.RunSubAgent`) — build a payload with no
client tools and attach no SSE client, so every device call fast-fails with
"cannot be executed by this client". The user's app is running and logged in;
nothing routes the call to it.

Goal: an app-wide, per-user device channel. The app advertises the device
tools it can execute; any run of that user — background included — can
advertise those tools to the LLM and dispatch calls to the app over the
status stream, blocking in place until the device answers.

## Roles

- **Claim**: a live advertisement `(userID, client_id, tools[], client_folder_path, expires_at)`.
  One active claim per user: the freshest non-expired claim wins. The claim
  lives exactly as long as its status-stream connection (plus TTL renewal).
- **Request**: one device tool call dispatched to the winning claim.
- **Result**: the device's answer, POSTed back, resolves the pending request.

## Endpoints (server)

### `GET /status/stream?client_id=<id>`
Existing endpoint, new optional query param. `client_id` is the app's
`ClientIdentity().id`. The server records it on the `StatusClient`. A status
client with an empty `client_id` is legacy: status events only, no device
channel. When a status client is removed, its claim is dropped.

### `POST /client/claim` (auth'd)
```json
{ "client_id": "abc123",
  "tools": [ { "name": "shell_client__exec", "description": "...", "parameters": {...} } ],
  "client_folder_path": "C:\\Code\\Outlook",
  "ttl_seconds": 120 }
```
→ `200 {"ok": true, "expires_at": "<RFC3339>"}`. Stores/refreshes the claim
for `(userID, client_id)`. `ttl_seconds` default 120, clamp 15..600. The app
re-claims every ~30s while its status stream is open. Claims whose TTL lapse
are treated as expired (lazy expiry check on read).

### `POST /chat/device-tool-result/<request_id>` (auth'd)
```json
{ "tool_call_id": "call_0", "name": "shell_client__exec",
  "result": "<tool output>", "error": "" }
```
→ `200 {"ok": true}` when a pending request of this user matched; `404`
when unknown/expired/belongs to another user (the app ignores the response).
`result` and `error` are concatenated server-side (`result` + `\n` + `error`
when both non-empty), matching how a chat tool result is stored.

## Status stream frames

`StatusEvent` gains:
- `type` — `""` (legacy status, unchanged) | `"device_tool_request"`
- `request_id`
- `device_tool` — object `{conversation_id, tool_call_id, name, arguments,
  timeout_seconds, source}` where `arguments` is the raw JSON string of args,
  `source` is `"background"` (cron/webhook/sub-agent) or `"chat"`.

Device requests are targeted: the server sends the frame only to status
clients whose `client_id` equals the claim's. Legacy clients never see them.

## Server dispatch (tool loop)

In `RunLLMLoop`, for a client tool call that has **no SSE client attached to
this conversation** (the background case) the loop consults the device
registry before fast-failing:

1. Tool must be advertised in `payload.ClientSideTools` (for background runs
   that means: it came from the live claim).
2. A live claim for `ar.UserID` must advertise the tool.
3. Tool mode in `payload.ModelSelected` is not `"ask"` — `"ask"` needs an
   interactive human, so a background dispatch synthesizes an explanatory
   error result instead of executing.

If all three hold, dispatch **in place** (the loop does not park and return):

- broadcast `tool_use` SSE (`is_server: false`) for any viewer/reconnect,
  and `BroadcastStatus("tool_use", name)` so the sidebar shows it,
- register a pending request, send the `device_tool_request` status frame,
- wait for: result | timeout (default 180s) | `ar.Ctx.Done()`,
- persist the tool result message (`db.PushMessage`) and broadcast
  `tool_result` exactly as a chat device call does,
- on timeout: synthesize an error result and arm the device-unresponsive
  memo (`clientToolTimeouts`) so the loop does not burn the whole budget
  again immediately — same semantics as the stall watchdog.

Device calls in one batch run **sequentially** (the device serialises shell
work), then the loop continues to the LLM with all results in place.

`clientToolUnreachable` gains a `hasDeviceClaim` term:
unadvertised → unreachable; advertised + no conversation client + no status
client + no claim → unreachable; advertised + claim → dispatchable. When a
claim exists, the reattach grace is not paid — the channel works without a
conversation stream.

## Background runs advertise the claim

`jobs.RunPrompt` / `jobs.RunSubAgent` populate `payload.ClientSideTools`
from `DeviceRegistry.ToolsForUser(userID)` (the live claim). A sub-agent also
intersects with the parent's advertised set (a sub-agent can never gain tools
its parent lacked). Consequence: a device tool is visible to a background run
only when (a) the user's app is live and claiming it, and (b) the run's
preset/policy enables that tool in `model.Tools`. `ai_tools.GetRequests`
already filters client tools by selection, using the namespaced key
(`shell_client__exec`), so preset tool config governs consent.

## Client (Flutter)

A dedicated always-on device channel, independent of the sidebar's
idle-closing status stream:

- open `/status/stream?client_id=<identity>` and keep it open,
- `POST /client/claim` on connect and every ~30s with the tool defs the app
  can execute *unattended* (`ShellService.getToolDefinition`,
  `FilesystemService` read/write defs when a folder is attached, registered
  MCP tool defs), plus the attached folder path,
- on `device_tool_request`: validate args against the advertised schema,
  execute through the same bounded executor the chat path uses
  (`runClientToolBounded*`), then POST the result. Never execute a tool the
  claim does not advertise.
- Surface it: show a "background run used your device" indicator so an
  unattended call is never invisible.

## Invariants

- A device tool never runs on the server. The server only routes.
- No claim → the existing fast-fail error text stays exactly as today.
- The claim is per-user and tied to a live connection; a stale claim cannot
  receive work (lazy expiry), so background runs fail fast, not hang.
- Chat-path dispatch is untouched. The device channel is only used when the
  loop has no conversation SSE client.
