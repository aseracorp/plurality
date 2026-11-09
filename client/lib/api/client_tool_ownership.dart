/// Which client runs a conversation's device-side (client) tools.
///
/// The rule lives here, separate from ChatService, so it is unit-testable and
/// so the two call sites (stream-driven dispatch and the resume-on-open path)
/// cannot drift apart.
///
/// Root cause this makes functional again: the client identity was persisted
/// through shared_preferences, which on web is **localStorage** — shared by
/// every tab of the browser. Tab1 and tab2 therefore had the SAME id, so
/// `lockId != myId` was never true, the "locked on X" chip could never appear,
/// and both tabs believed they owned client-tool execution. Identity is now
/// scoped per tab (sessionStorage), which makes this check meaningful again:
/// the holder runs the device-side tools, the other tab stays read-only for
/// tools.
///
/// The semantics are deliberately identical to the inline check this replaces,
/// so dispatch behaviour is unchanged beyond the lock becoming enforceable:
///  * identity not resolved yet (`myId` empty) → defer. Dispatching with an
///    empty id would claim a lock nobody can identify.
///  * a lock held by a different client → defer.
///  * unowned (`lockId == null`) → own. The resume path relies on this to pick
///    up tool calls left unanswered when the previous client crashed or was
///    closed mid-execution; that path only runs when there is no live stream,
///    so recovery is safe.
enum ClientToolOwnership {
  /// Run the tools here.
  own,

  /// Someone else owns them; do not dispatch.
  defer,
}

/// Decide whether THIS client should execute client tools.
///
/// [lockId] is the conversation's `clientLock.id` (null when nobody has
/// claimed it). [myId] is this client's identity, empty before init resolves.
ClientToolOwnership clientToolOwnership({
  required String? lockId,
  required String myId,
}) {
  if (myId.isEmpty) return ClientToolOwnership.defer;
  if (lockId == null) return ClientToolOwnership.own;
  return lockId == myId ? ClientToolOwnership.own : ClientToolOwnership.defer;
}
