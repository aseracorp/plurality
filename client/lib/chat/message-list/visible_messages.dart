import '../../utils/types.dart';
import 'wait_badge.dart' show isHiddenWaitResumeMessage;

/// Pure helper deciding which stored messages are rendered in the message
/// list, given what the live streaming session is currently showing.
///
/// The streaming section renders the in-flight assistant turn, so the stored
/// assistant message that turn corresponds to must be skipped — otherwise the
/// same reply renders twice.
///
/// Which stored message can that be? Only an assistant message that is the
/// FINAL entry of the conversation. The server persists an assistant message
/// when its stream completes, so if the newest entry is a user message the
/// live stream is a brand-new turn with no persisted counterpart, and nothing
/// may be dropped.
///
/// Earlier assistant messages must NEVER be dropped on the basis of the
/// streaming content. The previous implementation tested
/// `streamingText.contains(m.textContent)` against EVERY assistant message,
/// which hid perfectly normal older replies whose text happened to be a
/// substring of whatever was being streamed ("Yes.", "Done.", "Let me
/// check.") — they vanished mid-stream and only came back once the turn
/// finished and the snapshot reloaded. That is the reported "sometimes only
/// some chat messages in between are gone".
List<Message> selectVisibleMessages({
  required List<Message> messages,
  required bool hasStreamingContent,
  required String streamingText,
  required Set<String> streamingToolIds,
}) {
  // The live stream can only stand in for a trailing assistant message.
  final streamReplacesLast =
      hasStreamingContent &&
      messages.isNotEmpty &&
      messages.last.role == 'assistant';

  final visible = <Message>[];
  for (var i = 0; i < messages.length; i++) {
    final m = messages[i];
    if (m.role == 'tool') continue;
    // Synthetic "Timer is done" assistant messages exist only as a hook for
    // the LLM — they have no user-facing payload.
    if (isHiddenWaitResumeMessage(m)) continue;

    if (streamReplacesLast && i == messages.length - 1) {
      final hasText = m.textContent.isNotEmpty;
      final hasToolCalls = m.toolCalls != null && m.toolCalls!.isNotEmpty;
      final allToolsInStream = hasToolCalls &&
          m.toolCalls!.every((tc) => streamingToolIds.contains(tc.id));
      // Everything in this message is already rendered by the streaming
      // section → skip it, otherwise it shows up twice.
      final textInStream = hasText && streamingText.contains(m.textContent);
      if ((!hasText || textInStream) && (!hasToolCalls || allToolsInStream)) {
        continue;
      }
    }
    visible.add(m);
  }
  return visible;
}
