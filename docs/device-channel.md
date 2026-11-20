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

In `RunLLMLoop`, a client tool call whose tool is advertised by the user's
**live claim** is dispatched over the channel — with no conversation stream
attached (the background case, #196) **and** with one attached, when the
streaming client cannot run that tool:

1. Tool must be advertised in `payload.ClientSideTools` (for background runs
   that means: it came from the live claim).
2. A live claim for `ar.UserID` must advertise the tool.
3. `deviceChannelSteersCall` says the attached client is not the executor:
   no stream → steer; stream attached and the client advertised the tool →
   the chat path, byte-for-byte; stream attached and the client did NOT
   advertise the tool → steer (a web tab has no device shell at all, and a
   desktop window whose picker has the tool off cannot dispatch it either);
   `"ask"` mode with a stream attached → never steer, the approval prompt
   lives on the attached client.

If those hold, dispatch **in place** (the loop does not park and return):

- broadcast `tool_use` SSE (`is_server: false`, `device_dispatched: true`)
  for any viewer/reconnect, and `BroadcastStatus("tool_use", name)` so the
  sidebar shows it,
- mark the call in flight (`markDeviceDispatched`), register a pending
  request, send the `device_tool_request` status frame,
- wait for: result | timeout (default 180s) | `ar.Ctx.Done()`,
- persist the tool result message (`db.PushMessage`) and broadcast
  `tool_result` exactly as a chat device call does,
- on timeout: synthesize an error result and arm the device-unresponsive
  memo (`clientToolTimeouts`) so the loop does not burn the whole budget
  again immediately — same semantics as the stall watchdog.

Device calls in one batch run **sequentially** (the device serialises shell
work), then the loop continues to the LLM with all results in place.

### The chat case this fixes (2026-11-20)

Before this, the dispatch block was gated on `ar.ClientCount() == 0`, so a
chat turn could never use the channel. Production signature (conversation
`f533e88d`): the user chats in the **web** app, which advertises no device
shell (web has no `Process.start`); `ai_tools.GetRequests` still advertises
the Registry stub because the picker has `shell_client__exec: true`, the loop
parks on a client that can never run it, and every call costs a 90s stall
plus a watchdog timeout. `server.log` shows both shapes of one conversation:
`"1 connected clients"` + stall at 08:31/09:31, device-channel dispatch
answering fine at 08:33 with `"0 connected clients"`.

### Viewer echo guard

The `tool_use` of a dispatched call reaches every viewer of the conversation.
A viewer that is not the executing app validates it against ITS advertised
tools, fails, and POSTs the refusal back as a tool result — a `tool_results`
POST cancels the live loop to resume on it, tearing down the pending request
so the device's real answer lands on a 404. Two guards:

- the SSE event carries `device_dispatched: true` and ChatService does not
  queue such a call as its own `pendingClientTools`,
- `dropInFlightDeviceResults` in `HandleChat` discards a result posted for a
  call the live loop has marked in flight (`markDeviceDispatched` /
  `clearDeviceDispatched`), so the loop is never cancelled out from under the
  device. Cleared on every exit path, so a reused provider call id
  (`"call_0"`) is accepted again in the next turn.

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
- Chat-path dispatch is untouched **for the client that advertises the tool**:
  it keeps the broadcast/park/watchdog/approval path byte-for-byte. The device
  channel is used when no conversation SSE client is attached, and additionally
  when one is attached but cannot run the tool (it did not advertise it) and
  the tool is not in `"ask"` mode.
- A viewer must never answer for a call the channel is executing: the
  `device_dispatched` flag plus `dropInFlightDeviceResults` are the two sides
  of that rule.
