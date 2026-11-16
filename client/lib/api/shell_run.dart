import 'dart:async';
import 'dart:convert';
import 'dart:io';

/// Result of one bounded device-shell run.
class ShellRunResult {
  final int exitCode;
  final String stdout;
  final String stderr;
  final bool timedOut;
  final bool spawnFailed;
  final String spawnError;

  const ShellRunResult({
    this.exitCode = 0,
    this.stdout = '',
    this.stderr = '',
    this.timedOut = false,
    this.spawnFailed = false,
    this.spawnError = '',
  });
}

/// Spawn [executable] with [args], collect both streams, and return the
/// outcome — every stage bounded, so the caller's isolate is never parked.
///
/// The device shell tool runs on the UI isolate via ChatService's tool loop.
/// Three places can hang there, and on Windows they routinely do:
///
///  * `Process.start` itself. `powershell.exe` can be held by endpoint
///    security / group policy, and a `workingDirectory` on a
///    Tailscale/SMB/OneDrive share blocks while Windows renegotiates the
///    connection — the await never completes, so no tool result is ever sent.
///  * `proc.exitCode` with no bound. A child that ignores SIGTERM outlives the
///    kill, so that await never completes either.
///  * The pipe drains. A grandchild that inherits the handles keeps stdout open
///    after the parent exits, so `.join()` never completes — and with an
///    abandoned drain, the output the parent DID produce is lost.
///
/// All of these surface to the server as "Client did not respond within 90s"
/// (server/src/ai/tool_loop.go, `clientToolStallTimeout`), which resumes the
/// LLM loop while the UI isolate stays blocked — every later device call,
/// filesystem included, then times out as well. That is the "client froze"
/// report.
///
/// Output is therefore collected incrementally as chunks arrive, and the drain
/// gets only [drainGrace] after the child exits: a bounded wait returns the
/// output produced so far, so "partial output below" stays truthful instead of
/// discarding everything.
///
/// [timeout] bounds the whole run and stays below the server's 90s watchdog, so
/// the model receives this real result and can self-correct. [spawnTimeout]
/// bounds the spawn alone, so a hung `Process.start` answers in seconds.
Future<ShellRunResult> runCommandBounded(
  String executable,
  List<String> args, {
  String? workingDirectory,
  required Duration timeout,
  Duration spawnTimeout = const Duration(seconds: 10),
  Duration drainGrace = const Duration(seconds: 2),
}) async {
  Process proc;
  try {
    proc = await Process.start(
      executable,
      args,
      workingDirectory: workingDirectory == null || workingDirectory.isEmpty
          ? null
          : workingDirectory,
      runInShell: false,
    ).timeout(spawnTimeout);
  } on TimeoutException {
    return ShellRunResult(
      spawnFailed: true,
      spawnError:
          'could not spawn "$executable" within ${spawnTimeout.inSeconds}s '
          '(endpoint security, group policy, or a frozen/unavailable working '
          'directory such as a sleeping network share)',
    );
  } catch (e) {
    return ShellRunResult(spawnFailed: true, spawnError: e.toString());
  }

  // Collect incrementally. `.join()` hands over the full body only at stream
  // completion, which a surviving grandchild never reaches; and a drain
  // abandoned on timeout would discard everything read up to that point.
  final outBuf = StringBuffer();
  final errBuf = StringBuffer();
  final outDone = Completer<void>();
  final errDone = Completer<void>();

  void finish(Completer<void> c) {
    if (!c.isCompleted) c.complete();
  }

  proc.stdout
      .transform(utf8.decoder)
      .listen(
        outBuf.write,
        onError: (_) => finish(outDone),
        onDone: () => finish(outDone),
        cancelOnError: true,
      );
  proc.stderr
      .transform(utf8.decoder)
      .listen(
        errBuf.write,
        onError: (_) => finish(errDone),
        onDone: () => finish(errDone),
        cancelOnError: true,
      );

  var timedOut = false;
  int code;
  try {
    code = await proc.exitCode.timeout(
      timeout,
      onTimeout: () {
        timedOut = true;
        try {
          proc.kill(ProcessSignal.sigterm);
        } catch (_) {}
        return -1;
      },
    );
  } catch (_) {
    timedOut = true;
    code = -1;
  }

  // A child that traps/ignores SIGTERM keeps running and keeps the pipes open;
  // escalate so the drains can complete.
  if (timedOut) {
    try {
      proc.kill(ProcessSignal.sigkill);
    } catch (_) {}
  }

  // Grace, not a full wait: after exit (or the kill) the streams normally close
  // at once. A detached grandchild holds them open, and then only a short bound
  // keeps this call answerable — carrying the output collected up to then.
  Future<void> settled(Completer<void> c) =>
      c.future.timeout(drainGrace, onTimeout: () {});
  try {
    await Future.wait([settled(outDone), settled(errDone)]);
  } catch (_) {
    // Drains are best-effort; whatever was collected is the answer.
  }

  return ShellRunResult(
    exitCode: code,
    stdout: outBuf.toString(),
    stderr: errBuf.toString(),
    timedOut: timedOut,
  );
}
