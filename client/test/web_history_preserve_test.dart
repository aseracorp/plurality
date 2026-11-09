import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/service.dart';
import 'package:plurality/utils/types.dart';

Message _msg(String text, {String? id}) => Message(
      role: 'user',
      id: id,
      content: [ContentPart(type: 'text', text: text)],
    );

Conversation _conv(String id, List<Message> messages) => Conversation(
      id: id,
      title: 'Chat',
      lastMessageAt: DateTime(2026, 1, 1),
      modelSelected: ModelSelected(),
      messages: messages,
    );

/// Regression for Bug 2: in an EXISTING chat, sending a message made the whole
/// history AND the sent message vanish, leaving an empty chat.
///
/// On web Hive persists METADATA ONLY (`ConversationStorage._metaOnly` →
/// `messages: const []`). Every mutator in ConversationStorage reads that
/// metadata-only object back, mutates a field, and returns it. The notifier
/// published that returned object straight into `state.conversations`, which
/// replaced the in-memory conversation — the only place message history lives
/// on web — so the chat blanked. Two paths fire on a plain send: the optimistic
/// `addMessage` and the client-lock claim via `updateConversationMetaData`.
void main() {
  group('applyLocalUpdate (web metadata-only storage round-trip)', () {
    test('regression: send on web keeps history + the sent message', () {
      final history = [_msg('older'), _msg('mine')];
      final cached = _conv('c1', history);

      // What ConversationStorage.addMessage returns on web: the metadata-only
      // Hive object with the new message appended — i.e. NO prior history.
      final justSent = _msg('the message I just sent', id: 'm9');
      final fromStorage = _conv('c1', [justSent]);

      final merged = ConversationsNotifier.applyLocalUpdate(
        cached,
        fromStorage,
        append: justSent,
      );

      expect(merged.messages.length, 3,
          reason: 'history must survive the send, plus the new message');
      expect(merged.messages.map((m) => m.textContent).toList(),
          ['older', 'mine', 'the message I just sent']);
    });

    test('regression: lock/metadata update alone never blanks the chat', () {
      final cached = _conv('c1', [_msg('a'), _msg('b'), _msg('c')]);
      // updateConversationMetaData returns the metadata-only shell.
      final fromStorage = _conv('c1', []);
      fromStorage.modelSelected = ModelSelected(
        clientLock: const ClientLock(id: 'pc-1', label: 'pc-1'),
      );

      final merged =
          ConversationsNotifier.applyLocalUpdate(cached, fromStorage);

      expect(merged.messages.length, 3,
          reason: 'a metadata update must not drop message history');
      expect(merged.modelSelected.clientLock?.id, 'pc-1',
          reason: 'the metadata change itself is kept');
    });

    test('no duplicate when storage already contains the message (desktop)',
        () {
      final justSent = _msg('hi', id: 'm7');
      final cached = _conv('c1', [_msg('prior')]);
      final fromStorage = _conv('c1', [_msg('prior'), justSent]);

      final merged = ConversationsNotifier.applyLocalUpdate(
        cached,
        fromStorage,
        append: justSent,
      );

      expect(merged.messages.length, 2,
          reason: 'the append must not duplicate an already-persisted message');
    });

    test('dedupes by stable server id, not object identity', () {
      final cached = _conv('c1', [_msg('prior')]);
      final fromStorage = _conv('c1', [
        _msg('prior'),
        _msg('hi', id: 'm7'),
      ]);

      final merged = ConversationsNotifier.applyLocalUpdate(
        cached,
        fromStorage,
        append: _msg('hi', id: 'm7'), // equal id, different instance
      );

      expect(merged.messages.length, 2);
    });

    test('recency bumps only on a real append, not on metadata updates', () {
      final cached = _conv('c1', [_msg('a')]);
      final metadataOnly = _conv('c1', []);
      final storedAt = metadataOnly.lastMessageAt; // capture: mutated in place

      final untouched =
          ConversationsNotifier.applyLocalUpdate(cached, metadataOnly);
      expect(untouched.lastMessageAt, storedAt,
          reason: 'metadata update must not re-sort the sidebar');

      final bumped = ConversationsNotifier.applyLocalUpdate(
        cached,
        metadataOnly,
        append: _msg('new', id: 'm1'),
      );
      expect(bumped.lastMessageAt.isAfter(storedAt), isTrue);
    });

    test('unknown conversation (cached == null) is passed through safely', () {
      final fromStorage = _conv('c1', [_msg('only')]);
      final merged =
          ConversationsNotifier.applyLocalUpdate(null, fromStorage);
      expect(merged.messages.length, 1);
    });
  });
}
