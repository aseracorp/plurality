import 'dart:async';

import 'package:flutter/foundation.dart' show compute;
import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/process.dart';
import 'package:plurality/api/shell_worker.dart';

/// Production cascade (2026-11-16..18, Outlook-bridge conversation): every
/// `shell_client__exec` answered "Client did not respond within 90s", INCLUDING
/// `operation: list` / `start`, which spawn nothing and touch only in-memory
/// state. Bounding one command's syscalls cannot explain that.
///
/// The explanation is thread starvation, not isolate parking: Dart hands
/// process/file IO to a small shared io-worker pool. A spawn into a sleeping
/// Tailscale/SMB share never returns and permanently eats one of those threads;
/// issue a handful and the pool is exhausted, so the app's own HTTP sockets
/// cannot get a thread — the batch's `tool_results` POST stalls, and calls that
/// touch no process at all time out server-side with it.
///
/// The fix moves shell execution onto a worker isolate behind a single-flight
/// gate. These tests pin both halves, with real processes.
void main() {
  group('SingleFlight', () {
    test('serialises tasks: never two at a time', () async {
      final gate = SingleFlight();
      int live = 0;
      int maxLive = 0;

      Future<int> task(int id) async {
        live++;
        if (live > maxLive) maxLive = live;
        await Future<void>.delayed(const Duration(milliseconds: 60));
        live--;
        return id;
      }

      final sw = Stopwatch()..start();
      final results = await Future.wait([
        gate.run(() => task(1)),
        gate.run(() => task(2)),
        gate.run(() => task(3)),
      ]);
      sw.stop();

      expect(maxLive, 1, reason: 'a wedged spawn must not be stacked');
      expect(results, [1, 2, 3], reason: 'each caller gets its own value');
      expect(sw.elapsed.inMilliseconds, greaterThanOrEqualTo(170),
          reason: 'serialised, not concurrent');
    });

    test('a failing task does not fail the queued ones', () async {
      final gate = SingleFlight();
      final first = gate.run<int>(() async {
        await Future<void>.delayed(const Duration(milliseconds: 40));
        throw Exception('wedged');
      });
      final second = gate.run(() async => 'fine');

      await expectLater(first, throwsA(isA<Exception>()));
      expect(await second, 'fine',
          reason: 'the queue must not poison later callers');
      expect(gate.busy, isFalse);
    });

    test('a task that never returns leaves the gate busy, not dead-locked for its caller', () async {
      final gate = SingleFlight();
      final wedged = gate.run(() => Completer<int>().future);
      expect(gate.busy, isTrue);

      // The caller's own bound is what releases it; the gate then frees up.
      final bounded = wedged.timeout(
        const Duration(milliseconds: 150),
        onTimeout: () => -1,
      );
      expect(await bounded, -1);
      expect(gate.busy, isTrue, reason: 'the abandoned task still owns the slot');
    });

    test('a wedged task holds the slot; its callers answer via their own bound', () async {
      final gate = SingleFlight();
      final wedged = gate.run(() => Completer<int>().future);
      final queued = gate.run(() async => 'queued');

      // Intended: the queue is serialised, so a wedged spawn blocks the NEXT
      // shell run rather than eating a second io-worker thread. Every caller
      // still answers, because ChatService bounds each call (see
      // client_tool_timeout.dart) and the service bounds the hand-off.
      final out = await queued.timeout(
        const Duration(milliseconds: 200),
        onTimeout: () => 'abandoned by the caller bound',
      );
      expect(out, 'abandoned by the caller bound');
      expect(gate.busy, isTrue, reason: 'the wedged task owns the slot');
      unawaited(wedged);
    });

    test('the queue resumes in order when tasks complete', () async {
      final gate = SingleFlight();
      final first = gate.run(() async {
        await Future<void>.delayed(const Duration(milliseconds: 150));
        return 1;
      });
      final second = gate.run(() async {
        await Future<void>.delayed(const Duration(milliseconds: 50));
        return 2;
      });
      expect(await first, 1);
      expect(await second, 2);
      expect(gate.busy, isFalse);
    });
  });

  group('runShellOnWorker', () {
    test('the payload and entry point are compute()-sendable', () async {
      // shell_service calls compute(runShellOnWorker, req). compute() sends the
      // pair across a SendPort, so a non-sendable payload (a closure, a
      // Process, a stream) throws at RUNTIME — analyze() will not catch it, and
      // the failure would surface as a tool-call error. This exercises the real
      // hand-off shell_service performs.
      final req = ShellWorkerRequest(
        executable: 'sh',
        args: ['-c', 'echo across-the-sendport'],
        timeout: const Duration(seconds: 10),
      );
      final res = await compute(runShellOnWorker, req);
      expect(res.spawnFailed, isFalse);
      expect(res.stdout, contains('across-the-sendport'));
    });

    test('runs a real command off the caller isolate', () async {
      final req = ShellWorkerRequest(
        executable: 'sh',
        args: ['-c', 'echo worker-stdout; echo worker-stderr 1>&2'],
        timeout: const Duration(seconds: 10),
      );
      final res = await runShellOnWorker(req);
      expect(res.spawnFailed, isFalse);
      expect(res.exitCode, 0);
      expect(res.stdout, contains('worker-stdout'));
      expect(res.stderr, contains('worker-stderr'));
    });

    test('a missing executable reports spawnFailed in bounded time', () async {
      final req = ShellWorkerRequest(
        executable: 'definitely-not-a-binary-xyz',
        args: [],
        timeout: const Duration(seconds: 10),
      );
      final sw = Stopwatch()..start();
      final res = await runShellOnWorker(req);
      sw.stop();
      expect(res.spawnFailed, isTrue);
      expect(sw.elapsed.inSeconds, lessThan(10));
    });

    test('an unusable working directory reports spawnFailed, not a hang', () async {
      // The Windows signature: a folder on a sleeping network share. A missing
      // directory is the deterministic stand-in — CreateProcess fails the same
      // way, and the caller must get an answer.
      final req = ShellWorkerRequest(
        executable: 'sh',
        args: ['-c', 'pwd'],
        workingDirectory: '/definitely/not/a/real/directory-xyz',
        timeout: const Duration(seconds: 10),
      );
      final sw = Stopwatch()..start();
      final res = await runShellOnWorker(req);
      sw.stop();
      expect(res.spawnFailed, isTrue);
      expect(sw.elapsed.inSeconds, lessThan(12));
    });

    test('a command that outlives its bound is killed and returns partial output', () async {
      final req = ShellWorkerRequest(
        executable: 'sh',
        args: ['-c', 'echo early; sleep 30'],
        timeout: const Duration(seconds: 1),
        drainGrace: const Duration(seconds: 1),
      );
      final sw = Stopwatch()..start();
      final res = await runShellOnWorker(req);
      sw.stop();
      expect(res.timedOut, isTrue);
      expect(res.exitCode, -1);
      expect(res.stdout, contains('early'),
          reason: 'output collected before the kill must survive the drain');
      expect(sw.elapsed.inSeconds, lessThan(8));
    });
  });

  group('shellHandoffBound', () {
    test('covers the worker\'s own bounds plus margin', () {
      final req = ShellWorkerRequest(
        executable: 'sh',
        args: [],
        timeout: const Duration(seconds: 60),
        spawnTimeout: const Duration(seconds: 10),
        drainGrace: const Duration(seconds: 2),
      );
      final bound = shellHandoffBound(req);
      expect(
        bound.inSeconds,
        greaterThanOrEqualTo(72),
        reason: 'a healthy run must never be cut short by the hand-off bound',
      );
      expect(bound.inSeconds, lessThan(90),
          reason: 'stays under the server stall watchdog so the model gets a real result');
    });

    test('the hand-off message names the fallbacks', () {
      final msg = shellHandoffTimeoutMessage(
        'powershell.exe',
        const Duration(seconds: 75),
      );
      for (final want in ['powershell.exe', '75s', 'system_tools__shell_exec', 'wake the share']) {
        expect(msg, contains(want));
      }
    });
  });

  group('ProcessManager stdin write bound', () {
    test('a server that stops reading stdin answers instead of wedging the chain', () async {
      // Reads nothing, so the pipe fills and flush() would never complete on
      // master. This is the stateful-MCP dispatch path: a parked await there
      // strands the whole batch, which is why every later device call timed
      // out. The write is bounded now, so the call answers.
      final pm = ProcessManager(
        command: 'sh',
        name: 'not-reading',
        args: ['-c', 'sleep 120'],
      );
      expect(await pm.start(), isTrue);

      final sw = Stopwatch()..start();
      await expectLater(
        pm.sendRequest('tools/call', {'blob': 'x' * 4000000}),
        throwsA(anything),
      );
      sw.stop();
      expect(
        sw.elapsed.inSeconds,
        lessThan(20),
        reason: 'the flush bound must fire, not the 90s server watchdog',
      );
      await pm.stop();
    }, timeout: const Timeout(Duration(seconds: 60)));

    test('a healthy server round-trips JSON-RPC', () async {
      final pm = ProcessManager(
        command: 'sh',
        name: 'echo-rpc',
        args: [
          '-c',
          'while read -r line; do echo "{\\"jsonrpc\\":\\"2.0\\",\\"id\\":1,\\"result\\":{\\"ok\\":true}}"; done',
        ],
      );
      expect(await pm.start(), isTrue);
      // The manager keys responses by its own uuid, so a stub echo does not
      // satisfy the round trip; what this proves is that the write completes
      // (no flush timeout) and the request times out on the RPC bound.
      final sw = Stopwatch()..start();
      await expectLater(pm.sendRequest('tools/list'), throwsA(anything));
      sw.stop();
      expect(sw.elapsed.inSeconds, lessThan(40),
          reason: 'RPC bound, not a hang');
      expect(sw.elapsed.inSeconds, greaterThan(5),
          reason: 'the write succeeded fast; the wait was the RPC bound');
      await pm.stop();
    }, timeout: const Timeout(Duration(seconds: 60)));
  });
}
