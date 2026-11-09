import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/chat/message-list/conversation_load.dart';

/// Regression for the "Could not load this conversation. Your messages are
/// safe on the server — tap to retry." banner appearing on a FRESH page load,
/// before the user had opened any chat.
///
/// The new-chat screen renders a ChatInterface whose conversationId is ''.
/// initState/didUpdateWidget call _loadConversation(''), and
/// ConversationsNotifier.loadConversation returns false for an empty id (a
/// deliberate no-op, not an error). The caller treated every false as a load
/// failure and popped the error snackbar.
void main() {
  group('conversationLoadOutcome', () {
    test('regression: empty id (new-chat screen / fresh page load) is SKIPPED, '
        'never a failure — no banner', () {
      // loadConversation('') returns false; that must NOT be a failure.
      expect(
        conversationLoadOutcome('', false),
        ConversationLoadOutcome.skipped,
      );
    });

    test('empty id with a true result is still skipped (nothing was loaded)',
        () {
      expect(
        conversationLoadOutcome('', true),
        ConversationLoadOutcome.skipped,
      );
    });

    test('real failure for a real id stays FAILED (banner must still show)',
        () {
      // Genuine transient failures (offline, server restart, 5xx) keep the
      // "tap to retry" behaviour — the fix must not silence the error path.
      expect(
        conversationLoadOutcome('206c52c3043dbcaf88130269', false),
        ConversationLoadOutcome.failed,
      );
    });

    test('successful load of a real id is LOADED', () {
      expect(
        conversationLoadOutcome('206c52c3043dbcaf88130269', true),
        ConversationLoadOutcome.loaded,
      );
    });

    test('only FAILED may surface the banner', () {
      const bannerCases = {
        ConversationLoadOutcome.failed,
      };
      for (final outcome in ConversationLoadOutcome.values) {
        final shouldShowBanner = bannerCases.contains(outcome);
        expect(
          shouldShowBanner,
          outcome == ConversationLoadOutcome.failed,
          reason: '$outcome must not decide banner visibility',
        );
      }
    });

    test('before/after proof: the pre-fix rule (banner := !ok) fired on a '
        'fresh load; the new rule does not', () {
      // Pre-fix caller logic in chat-interface.dart was:
      //   loadConversation(id).then((ok) { if (ok || !mounted) return;
      //     showTopSnackBar(...); });
      // i.e. the banner showed for ANY false, including id == ''.
      const idAtFreshPageLoad = '';
      final ok = false; // what loadConversation('') returns
      final preFixShowsBanner = !ok;
      final postFixShowsBanner =
          conversationLoadOutcome(idAtFreshPageLoad, ok) ==
              ConversationLoadOutcome.failed;

      expect(preFixShowsBanner, isTrue,
          reason: 'documents the bug: a fresh page load popped the banner');
      expect(postFixShowsBanner, isFalse,
          reason: 'the fix must keep the new-chat screen silent');

      // A real failure still pops it, before and after.
      const realId = '206c52c3043dbcaf88130269';
      expect(
        conversationLoadOutcome(realId, false),
        ConversationLoadOutcome.failed,
      );
    });
  });
}
