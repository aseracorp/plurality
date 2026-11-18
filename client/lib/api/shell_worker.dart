import 'dart:async';

import 'shell_run.dart';

/// Payload for one bounded shell run executed on a worker isolate.
///
/// `compute()` sends this across a SendPort, so it must contain nothing but
/// value types — no closures, no `Process`, no streams.
class ShellWorkerRequest {
  final String executable;
  final List<String> args;
  final String? workingDirectory;
  final Duration timeout;
  final Duration spawnTimeout;
  final Duration drainGrace;

  const ShellWorkerRequest({
    required this.executable,
    required this.args,
    this.workingDirectory,
    required this.timeout,
    this.spawnTimeout = const Duration(seconds: 10),
    this.drainGrace = const Duration(seconds: 2),
  });
}

/// Runs one bounded shell command on a WORKER isolate.
///
/// Top-level: that is the shape `compute()` requires. Everything it touches
/// (`Process.start`, the exit-code await, the pipe drains) happens off the UI
/// isolate, so a wedged spawn can no longer park the app.
Future<ShellRunResult> runShellOnWorker(ShellWorkerRequest req) async {
  return runCommandBounded(
    req.executable,
    req.args,
    workingDirectory: req.workingDirectory,
    timeout: req.timeout,
    spawnTimeout: req.spawnTimeout,
    drainGrace: req.drainGrace,
  );
}

/// Wall-clock bound the CALLER puts on the hand-off, on top of the run bound
/// inside the worker.
///
/// `compute()`'s round trip is not free: the message crosses a SendPort, and
/// the worker's own bounds (spawn, run, drain) are enforced inside it, not on
/// the transfer. A wedged worker keeps running — its spawn never completed,
/// nothing owns it — so the caller must stop waiting for it. Giving it
/// run bound + spawn bound + drain grace + a margin means a healthy run is
/// never cut short, and a wedged one answers in bounded time.
Duration shellHandoffBound(ShellWorkerRequest req) =>
    req.timeout + req.spawnTimeout + req.drainGrace + const Duration(seconds: 5);

/// Message the caller returns when the worker itself never answers.
String shellHandoffTimeoutMessage(String executable, Duration bound) =>
    'Error: the device shell worker did not answer within ${bound.inSeconds}s '
    'while running "$executable". The spawn is wedged (endpoint security, group '
    'policy, or a working directory on a sleeping network share). The app stays '
    'responsive: the command runs on its own isolate and is abandoned here. Do '
    'NOT retry it in this conversation - use a server-side tool '
    '(system_tools__shell_exec, filesystem_server__fs_read) or ask the user to '
    'wake the share / restart the app.';

/// Runs tasks one at a time, in arrival order, each seeing its own result.
///
/// Not a nicety: Dart hands file and process IO to a small shared io-worker
/// pool (as few as 4 threads on the VM's default configuration). A spawn into a
/// sleeping Tailscale/SMB/OneDrive share never returns and permanently eats one
/// of those threads. Issue a handful and the pool is exhausted — the app's own
/// HTTP sockets and file IO can no longer get a thread, so the `tool_results`
/// POST of every queued tool batch stalls, and device calls that touch no
/// process at all (`operation: list`, `operation: status`) time out
/// server-side with it. That is the observed cascade where one hung command
/// made the entire client dead.
///
/// Serialising shell work commits at most one thread to a spawn, so the pool
/// always has threads left for the app's own IO. Failures are never propagated
/// to a queued caller: each task's result belongs to its own caller only.
class SingleFlight {
  Future<void>? _tail;

  bool get busy => _tail != null;

  Future<T> run<T>(Future<T> Function() task) async {
    final previous = _tail;
    final settled = Completer<void>();
    _tail = settled.future;
    if (previous != null) {
      await previous.then((_) {}, onError: (_) {});
    }
    try {
      return await task();
    } finally {
      if (!settled.isCompleted) settled.complete();
      if (identical(_tail, settled.future)) _tail = null;
    }
  }
}
