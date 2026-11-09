import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'package:plurality/api/client_scope_stub.dart';
import 'package:plurality/api/client_tool_ownership.dart';

/// Bug 3(b): Simplify open in a second window freezes the tab.
///
/// Root cause chain:
///  1. The client identity was persisted through shared_preferences, which on
///     web is localStorage — SHARED by every tab of the browser. Tab1 and tab2
///     therefore had the SAME id.
///  2. With one id, the lock arbitration `lockId != myId` is never true, so the
///     "locked on X" chip can never appear and both tabs believe they own
///     client-tool execution.
///  3. Both tabs then dispatch the same client tool calls (stream dispatch and
///     the resume-on-open path) and both drive their own SSE reconnect for the
///     same conversation. The double-dispatch guards (_activeStreams,
///     _resumingClientTools, the lock check) are per-process, so tabs cannot
///     see each other. That duplicated streaming + tool work is what stalls
///     the second window.
///
/// The fix scopes identity per tab (sessionStorage on web), which makes the
/// existing lock design work. The rules below are the arbitration it restores;
/// they are deliberately identical to the inline check they replace, so
/// recovery of orphaned tool calls is preserved.
void main() {
  group('clientToolOwnership', () {
    const me = 'tab-a-id';
    const other = 'tab-b-id';

    test('regression: a lock held by ANOTHER tab defers here', () {
      // Impossible to trigger before the fix (both tabs shared one id).
      expect(
        clientToolOwnership(lockId: other, myId: me),
        ClientToolOwnership.defer,
      );
    });

    test('a lock held by ME runs here', () {
      expect(
        clientToolOwnership(lockId: me, myId: me),
        ClientToolOwnership.own,
      );
    });

    test('unowned conversation: run here — this is the crash-recovery path',
        () {
      // The resume path relies on this to pick up tool calls left unanswered
      // when the previous client crashed / the tab was closed mid-execution.
      expect(
        clientToolOwnership(lockId: null, myId: me),
        ClientToolOwnership.own,
      );
    });

    test('identity not resolved yet: defer (never dispatch, never claim)', () {
      // ClientIdentity.id is '' before init(); dispatching with an empty id
      // would claim a lock nobody can identify.
      expect(
        clientToolOwnership(lockId: null, myId: ''),
        ClientToolOwnership.defer,
      );
      expect(
        clientToolOwnership(lockId: other, myId: ''),
        ClientToolOwnership.defer,
      );
    });

    test('two tabs of one browser, one lock holder: exactly one runs', () {
      const tab1 = 'tab-1';
      const tab2 = 'tab-2';
      final tab1Runs = clientToolOwnership(lockId: tab1, myId: tab1) ==
          ClientToolOwnership.own;
      final tab2Runs = clientToolOwnership(lockId: tab1, myId: tab2) ==
          ClientToolOwnership.own;
      expect(tab1Runs, isTrue);
      expect(tab2Runs, isFalse,
          reason: 'exactly one client may run the tools for a locked chat');
    });

    test('empty lock id on the holder side never matches (no phantom owner)',
        () {
      expect(
        clientToolOwnership(lockId: '', myId: me),
        ClientToolOwnership.defer,
      );
    });
  });

  group('scoped identity store (native stub)', () {
    test('returns null when unset, and round-trips a value', () async {
      SharedPreferences.setMockInitialValues({});
      expect(await readScopedId('client_identity_id'), isNull);

      await writeScopedId('client_identity_id', 'tab-a-id');
      expect(await readScopedId('client_identity_id'), 'tab-a-id');
    });

    test('scoped per key: two keys do not collide', () async {
      SharedPreferences.setMockInitialValues({'a': '1'});
      expect(await readScopedId('a'), '1');
      expect(await readScopedId('b'), isNull);
    });
  });
}
