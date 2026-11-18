import 'dart:async';

import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/process.dart';

/// `ProcessManager.start()` is awaited from the UI isolate: `initMCP()` runs it
/// for every configured server at app startup, and the stateful-MCP tool path
/// runs it lazily on dispatch. Two calls there were unbounded on Windows —
/// `Process.run('where', [cmd])` and `Process.start` — and when either parked
/// (endpoint security holding `where.exe`, a binary on a sleeping
/// Tailscale/SMB/OneDrive share) the UI isolate parked with it. No frames, no
/// SSE reads, no tool dispatch: every device tool call, including ones that
/// spawn nothing, then times out server-side with "Client did not respond
/// within 90s". That is the "the client froze" report, and the only recovery
/// was killing the app.
///
/// These run real processes so they prove the bounds are honoured, not just
/// that the code compiles.
void main() {
  test('a child process starts, is running, and stops cleanly', () async {
    final pm = ProcessManager(
      command: 'sh',
      name: 'sleeper',
      args: ['-c', 'echo ready; sleep 30'],
    );
    expect(await pm.start(), isTrue);
    expect(pm.isRunning, isTrue);
    await pm.stop();
    expect(pm.isRunning, isFalse);
  }, timeout: const Timeout(Duration(seconds: 45)));

  test('a server that never answers is bounded by requestTimeout', () async {
    // Consumes stdin and writes nothing: the JSON-RPC completer never
    // resolves, so the request must hit its bound. `cat` would echo the
    // request back and the parser rejects that immediately — the "child
    // exits" test covers that path.
    final pm = ProcessManager(
      command: 'sh',
      name: 'silent',
      args: ['-c', 'while read -r line; do :; done'],
    );
    expect(await pm.start(), isTrue);
    final sw = Stopwatch()..start();
    await expectLater(
      pm.sendRequest('initialize'),
      throwsA(isA<TimeoutException>()),
    );
    sw.stop();
    expect(
      sw.elapsed.inSeconds,
      lessThanOrEqualTo(ProcessManager.requestTimeout.inSeconds + 3),
      reason: 'a stalled MCP server must answer with an error, not park',
    );
    expect(
      sw.elapsed.inSeconds,
      greaterThanOrEqualTo(ProcessManager.requestTimeout.inSeconds - 1),
      reason: 'the bound is the request timeout, not an instant failure',
    );
    await pm.stop();
  }, timeout: const Timeout(Duration(seconds: 60)));

  test('a missing executable fails fast, start() does not park', () async {
    final pm = ProcessManager(
      command: 'definitely-not-a-binary-xyz',
      name: 'missing',
      args: [],
    );
    final sw = Stopwatch()..start();
    expect(await pm.start(), isFalse);
    sw.stop();
    expect(
      sw.elapsed.inSeconds,
      lessThan(10),
      reason: 'start() must not park the UI isolate',
    );
  });

  test('stop() returns promptly even for a child that ignores SIGTERM', () async {
    final pm = ProcessManager(
      command: 'sh',
      name: 'stubborn',
      args: ['-c', 'trap "" TERM; while true; do sleep 1; done'],
    );
    expect(await pm.start(), isTrue);
    final sw = Stopwatch()..start();
    await pm.stop();
    sw.stop();
    expect(sw.elapsed.inSeconds, lessThan(5));
  }, timeout: const Timeout(Duration(seconds: 45)));

  test('sendRequest on a process that never started throws, not hangs', () async {
    final pm = ProcessManager(command: 'nope-xyz', name: 'dead', args: []);
    await pm.start();
    expect(pm.isRunning, isFalse);
    await expectLater(pm.sendRequest('tools/list'), throwsA(anything));
  });

  test('a child exiting completes pending requests with an error', () async {
    final pm = ProcessManager(
      command: 'sh',
      name: 'dies',
      args: ['-c', 'echo starting; sleep 0.3; exit 7'],
    );
    expect(await pm.start(), isTrue);
    // The exit handler completes pending completers with an error; the
    // existing contract reports it as the 'Process terminated' string.
    await expectLater(pm.sendRequest('tools/list'), throwsA(anything));
    await pm.stop();
  }, timeout: const Timeout(Duration(seconds: 45)));
}
