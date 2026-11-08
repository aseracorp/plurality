// Web implementation of the scoped-id store: sessionStorage, i.e. PER TAB.
// Imported on web builds only (conditional import in client_identity.dart).
//
// SharedPreferences on web is backed by localStorage, which is SHARED by every
// tab of the browser. A client identity stored there is therefore IDENTICAL in
// tab1 and tab2, so the client-lock arbitration (`lock.id != myId`) can never
// distinguish the two windows: both tabs believe they own client-tool
// execution, both re-dispatch the trailing tool calls of the same turn, and
// both drive their own SSE reconnect for the same conversation. The guards
// that prevent double-dispatch (_activeStreams, _resumingClientTools, the
// lock check) are per-process, so two tabs cannot see each other. That
// duplicated streaming + tool work is what freezes the second window.
//
// sessionStorage is scoped to one tab, giving every window its own identity, so
// the existing lock/banner design works as intended: the holder runs the
// device-side tools, the other tab shows the "locked on X" chip and stays
// read-only for tools.
import 'package:web/web.dart' as web;

Future<String?> readScopedId(String key) async {
  final value = web.window.sessionStorage.getItem(key);
  return (value == null || value.isEmpty) ? null : value;
}

Future<void> writeScopedId(String key, String value) async {
  web.window.sessionStorage.setItem(key, value);
}
