import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/shell_run.dart';

const short = Duration(seconds: 2);

/// The device shell tool ran `Process.start`, `proc.exitCode` and the pipe
/// drains unbounded on the UI isolate. Any of the three can hang forever —
/// powershell.exe held by endpoint security, a `pwd` on a sleeping network
/// share, a child that ignores SIGTERM, a grandchild inheriting the stdout
/// handle — and a hang there means no tool result is ever sent. The server then
/// reports "Client did not respond within 90s" and every device call after it
/// fails too: the "client froze" report.
///
/// `runCommandBounded` bounds each stage. These run real processes, so they
/// prove the bounds are honoured, not just that the code compiles.
void main() {
  test('a normal command exits 0 with its stdout', () async {
    final r = await runCommandBounded('echo', ['hi'], timeout: short);
    expect(r.spawnFailed, isFalse);
    expect(r.timedOut, isFalse);
    expect(r.exitCode, 0);
    expect(r.stdout.trim(), 'hi');
  });

  test('a nonzero exit code is reported, not swallowed', () async {
    final r = await runCommandBounded('sh', ['-c', 'exit 3'], timeout: short);
    expect(r.exitCode, 3);
    expect(r.timedOut, isFalse);
  });

  test('a missing binary is a result, not a throw', () async {
    final r = await runCommandBounded(
      'definitely-not-a-binary-xyz',
      [],
      timeout: short,
    );
    expect(r.spawnFailed, isTrue);
    expect(r.spawnError, isNotEmpty);
  });

  test('a long-running child is bounded and killed', () async {
    final sw = Stopwatch()..start();
    final r = await runCommandBounded('sleep', [
      '30',
    ], timeout: const Duration(seconds: 1));
    sw.stop();
    expect(r.timedOut, isTrue);
    expect(r.exitCode, -1);
    expect(
      sw.elapsed.inSeconds,
      lessThan(5),
      reason: 'the tool call must answer, not park the isolate',
    );
  });

  test('a child trapping SIGTERM is escalated to SIGKILL', () async {
    final sw = Stopwatch()..start();
    final r = await runCommandBounded('sh', [
      '-c',
      'trap "" TERM; sleep 30',
    ], timeout: const Duration(seconds: 1));
    sw.stop();
    expect(r.timedOut, isTrue);
    expect(
      sw.elapsed.inSeconds,
      lessThan(6),
      reason: 'SIGKILL escalation must not wait out the child',
    );
  });

  test(
    'a detached grandchild holding the pipe still returns its output',
    () async {
      final sw = Stopwatch()..start();
      // The parent exits at once; the background grandchild inherits stdout and
      // keeps it open. An unbounded .join() never completes, and a drain
      // abandoned on timeout loses the output the parent produced.
      final r = await runCommandBounded('sh', [
        '-c',
        'sh -c "sleep 30" & echo hi',
      ], timeout: const Duration(seconds: 1));
      sw.stop();
      expect(sw.elapsed.inSeconds, lessThan(6));
      expect(r.exitCode, 0);
      expect(r.stdout, contains('hi'));
    },
  );

  test('stderr is captured separately', () async {
    final r = await runCommandBounded('sh', [
      '-c',
      'echo oops 1>&2',
    ], timeout: short);
    expect(r.stderr.trim(), 'oops');
    expect(r.stdout.trim(), isEmpty);
  });
}
