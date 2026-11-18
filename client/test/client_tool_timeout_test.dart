import 'dart:async';

import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/client_tool_timeout.dart';

/// ChatService runs client tool calls sequentially and posts the batch's
/// `tool_results` only after the last call returns. One call that never
/// returns therefore strands the WHOLE batch: the POST is never sent, the
/// server's 90s stall watchdog synthesizes a timeout, and every later device
/// call in the conversation times out too — including trivial ones queued
/// behind the wedged call. That is the "the client froze" report.
///
/// The services bound their own syscalls, but a service is more than its
/// syscalls (`retrieve_skill` awaits path_provider + readAsString unbounded).
/// These tests pin the outer bound the dispatch loop relies on.
void main() {
  test('a call that answers normally passes its result through', () async {
    final out = await runClientToolBounded(
      'shell_client__exec',
      () async => 'real output',
      timeout: const Duration(seconds: 5),
    );
    expect(out, 'real output');
  });

  test('a call that never returns is abandoned with an actionable error', () async {
    final sw = Stopwatch()..start();
    final out = await runClientToolBounded(
      'shell_client__exec',
      () => Completer<String>().future, // never completes
      timeout: const Duration(seconds: 1),
    );
    sw.stop();
    expect(sw.elapsed.inSeconds, lessThan(3));
    expect(out, contains('shell_client__exec'));
    expect(out, contains('did not finish within 1s'));
    expect(out, contains('Do NOT retry'));
    expect(out, contains('system_tools__shell_exec'));
  });

  test('a service failure is rethrown so the loop reports it verbatim', () async {
    // ChatService wraps each call in try/catch and posts 'Error: $e'. The
    // helper must not swallow it into a timeout-shaped message, or the model
    // loses the real reason (fs_ops returns "Error: path is outside...").
    await expectLater(
      runClientToolBounded(
        'shell_client__exec',
        () async => throw Exception('boom'),
        timeout: const Duration(seconds: 5),
      ),
      throwsA(isA<Exception>()),
    );
  });

  test('json variant encodes the response, and bounds a stalled one', () async {
    final ok = await runClientToolBoundedJson(
      'mail__mail_list',
      () async => {'items': [1, 2]},
      timeout: const Duration(seconds: 5),
    );
    expect(ok, '{"items":[1,2]}');

    final stalled = await runClientToolBoundedJson(
      'mail__mail_list',
      () => Completer<dynamic>().future,
      timeout: const Duration(seconds: 1),
    );
    expect(stalled, contains('did not finish within 1s'));
  });

  test('the bound sits below the server stall watchdog (90s)', () {
    expect(
      clientToolCallTimeout.inSeconds,
      lessThan(90),
      reason: 'the model must receive this real result, not the server-side '
          '"Client did not respond within 90s" stall',
    );
  });

  test('an abandoned call is not awaited: the loop keeps moving', () async {
    var laterRan = false;
    final wedged = runClientToolBounded(
      'fs_read',
      () => Completer<String>().future,
      timeout: const Duration(milliseconds: 200),
    );
    // The dispatch loop's next call must not be blocked by the wedged one.
    final next = await runClientToolBounded(
      'fs_read',
      () async {
        laterRan = true;
        return 'second result';
      },
      timeout: const Duration(seconds: 2),
    );
    expect(await wedged, contains('abandoned'));
    expect(next, 'second result');
    expect(laterRan, isTrue);
  });
}
