/// Outcome of a conversation-load attempt, decided from the requested id and
/// what the notifier reported back.
///
/// Extracted from ChatInterface so the distinction between "there is nothing
/// to load" and "the load failed" is unit-testable. Conflating the two is what
/// made a fresh page load show "Could not load this conversation": the new-chat
/// screen has an EMPTY conversation id, `loadConversation('')` returns false
/// (it is a deliberate no-op), and the caller read that false as a failure.
enum ConversationLoadOutcome {
  /// No load was needed (empty id — the new-chat screen). Must stay silent.
  skipped,

  /// History fetched (or merged) successfully.
  loaded,

  /// A genuine failure: the caller should surface the retry banner.
  failed,
}

/// Decide what the UI should do after `loadConversation(id)` completes.
///
/// [id] is the conversation the widget asked for, [serverSucceeded] is the
/// bool returned by `ConversationsNotifier.loadConversation`. An empty id is
/// NEVER a failure — there is simply no conversation to load yet.
ConversationLoadOutcome conversationLoadOutcome(
  String id,
  bool serverSucceeded,
) {
  if (id.isEmpty) return ConversationLoadOutcome.skipped;
  return serverSucceeded
      ? ConversationLoadOutcome.loaded
      : ConversationLoadOutcome.failed;
}
