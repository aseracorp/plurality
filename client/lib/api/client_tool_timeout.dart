import 'dart:async';
import 'dart:convert';

/// Bound on ONE client-side tool call inside ChatService's dispatch loop.
///
/// The loop runs tool calls sequentially and posts the batch's `tool_results`
/// only after the LAST call finishes. A call that never returns therefore does
/// not just hang itself: the POST is never sent, the server's stall watchdog
/// (server/src/ai/tool_loop.go, `clientToolStallTimeout` = 90s) synthesizes a
/// timeout and resumes the loop, and the results that eventually DO arrive are
/// stranded on a dead request. The user sees "the client froze", and every
/// later device call in the conversation times out as well — including calls
/// that do trivial work, because they sit behind the wedged one.
///
/// Individual services bound their own syscalls (`shell_run.dart`,
/// `filesystem_service.dart`, `process.dart`), but a service is more than its
/// syscalls: `retrieve_skill` awaits `getSkillsPath()` (path_provider) and
/// `File.readAsString()` with no bound at all, and any of those can park on a
/// sleeping Tailscale/SMB/OneDrive share. This is the outer bound that makes
/// the batch answerable no matter what a single call does.
///
/// Kept below the server's 90s watchdog so the model receives this real
/// result, not a stall.
const Duration clientToolCallTimeout = Duration(seconds: 70);

/// Abandoned calls keep running in the background (a `compute()` isolate, a
/// child process). The services own their own bounds and cleanup; the batch
/// just stops waiting on them.
String clientToolTimeoutMessage(String toolName, Duration timeout) =>
    'Error: tool "$toolName" did not finish within ${timeout.inSeconds}s and was abandoned. '
    'The device cannot answer this call right now (a sleeping Tailscale/SMB/OneDrive '
    'share, a blocked binary, or a wedged app). Do NOT retry it in this conversation: '
    'use a server-side tool (system_tools__shell_exec, filesystem_server__fs_read) or '
    'ask the user to wake the share / restart the app.';

/// Await [task] bounded by [timeout]. Returns the task's result, or an
/// actionable error when the bound is hit — never a hang.
Future<String> runClientToolBounded(
  String toolName,
  Future<String> Function() task, {
  Duration timeout = clientToolCallTimeout,
}) async {
  final work = task();
  try {
    return await work.timeout(timeout);
  } on TimeoutException {
    return clientToolTimeoutMessage(toolName, timeout);
  }
}

/// Same bound for calls whose result is a JSON object (MCP `tools/call`),
/// encoded the way the dispatch loop expects.
Future<String> runClientToolBoundedJson(
  String toolName,
  Future<dynamic> Function() task, {
  Duration timeout = clientToolCallTimeout,
}) async {
  final work = task();
  try {
    return jsonEncode(await work.timeout(timeout));
  } on TimeoutException {
    return clientToolTimeoutMessage(toolName, timeout);
  }
}

