import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/service.dart';
import 'package:plurality/api/storage.dart';
import 'package:plurality/chat/message-list/visible_messages.dart';
import 'package:plurality/utils/types.dart';

Message _msg(String role, String text, {
  String? id,
  String? toolCallId,
  List<ToolCall>? toolCalls,
}) {
  return Message(
    role: role,
    id: id,
    toolCallId: toolCallId,
    toolCalls: toolCalls,
    content: [ContentPart(type: 'text', text: text)],
  );
}

ToolCall _tool(String id, {String name = 'shell_client__exec'}) {
  return ToolCall(
    id: id,
    function: FunctionCallData(name: name, arguments: '{}'),
  );
}

Conversation _conv(
  String id, {
  String title = 'A',
  DateTime? at,
  List<Message> messages = const [],
}) {
  return Conversation(
    id: id,
    title: title,
    lastMessageAt: at ?? DateTime(2026, 1, 1),
    modelSelected: ModelSelected(),
    messages: messages,
  );
}

void main() {
  group('selectVisibleMessages', () {
    test('drops tool-result messages', () {
      final visible = selectVisibleMessages(
        messages: [
          _msg('user', 'hi'),
          _msg('tool', 'result', toolCallId: 'x'),
        ],
        hasStreamingContent: false,
        streamingText: '',
        streamingToolIds: {},
      );
      expect(visible.length, 1);
      expect(visible.first.role, 'user');
    });

    test('regression: older assistant reply that is a substring of the '
        'streaming text is NOT hidden', () {
      final messages = [
        _msg('user', 'are you there?', id: '1'),
        _msg('assistant', 'Yes.', id: '2'),
        _msg('user', 'say it again', id: '3'),
      ];
      // The live stream "Yes. I am here..." CONTAINS the older assistant
      // message "Yes.". The old filter dropped every such message, so history
      // entries "in between" vanished mid-stream and reappeared on reload.
      final visible = selectVisibleMessages(
        messages: messages,
        hasStreamingContent: true,
        streamingText: 'Yes. I am here and happy to help.',
        streamingToolIds: {},
      );
      expect(visible.map((m) => m.textContent), [
        'are you there?',
        'Yes.',
        'say it again',
      ]);
    });

    test('the last assistant message fully covered by the stream is skipped', () {
      final visible = selectVisibleMessages(
        messages: [
          _msg('user', 'hi', id: '1'),
          _msg('assistant', 'Hello there', id: '2'),
        ],
        hasStreamingContent: true,
        streamingText: 'Hello there, how can I help?',
        streamingToolIds: {},
      );
      expect(visible.map((m) => m.textContent), ['hi']);
    });

    test('last assistant message NOT yet in the stream stays visible', () {
      final visible = selectVisibleMessages(
        messages: [
          _msg('user', 'hi', id: '1'),
          _msg('assistant', 'Earlier reply', id: '2'),
        ],
        hasStreamingContent: true,
        streamingText: 'A complet',
        streamingToolIds: {},
      );
      expect(visible.map((m) => m.textContent), ['hi', 'Earlier reply']);
    });

    test('assistant message whose tool calls are all streaming is skipped', () {
      final visible = selectVisibleMessages(
        messages: [_msg('assistant', '', id: '1', toolCalls: [_tool('call_a')])],
        hasStreamingContent: true,
        streamingText: 'working',
        streamingToolIds: {'call_a'},
      );
      expect(visible, isEmpty);
    });

    test('assistant message with an un-streamed tool call stays visible', () {
      final visible = selectVisibleMessages(
        messages: [_msg('assistant', '', id: '1', toolCalls: [_tool('call_a')])],
        hasStreamingContent: true,
        streamingText: 'working',
        streamingToolIds: {},
      );
      expect(visible.length, 1);
    });

    test('no streaming content renders everything (tool results excluded)', () {
      final visible = selectVisibleMessages(
        messages: [
          _msg('user', 'a', id: '1'),
          _msg('assistant', 'b', id: '2'),
          _msg('user', 'c', id: '3'),
          _msg('assistant', 'Yes.', id: '4'),
        ],
        hasStreamingContent: false,
        streamingText: '',
        streamingToolIds: {},
      );
      expect(visible.length, 4);
    });

    test('empty conversation renders nothing', () {
      expect(
        selectVisibleMessages(
          messages: [],
          hasStreamingContent: true,
          streamingText: 'streaming',
          streamingToolIds: {},
        ),
        isEmpty,
      );
    });
  });

  group('mergeServerListWithCache', () {
    test('regression: server list (messages stripped) keeps cached messages', () {
      final cached = [
        _conv('a', messages: [_msg('user', 'hello', id: '1')]),
      ];
      // GET /conversations returns metadata only — messages are always empty.
      final fresh = [_conv('a', title: 'A (renamed)', at: DateTime(2026, 1, 2))];

      final merged = ConversationsNotifier.mergeServerListWithCache(cached, fresh);

      expect(merged.single.title, 'A (renamed)');
      expect(merged.single.messages.length, 1,
          reason: 'a list refresh must never blank message history');
      expect(merged.single.messages.first.textContent, 'hello');
    });

    test('server messages win when the server actually sent them', () {
      final merged = ConversationsNotifier.mergeServerListWithCache(
        [_conv('a', messages: [_msg('user', 'stale', id: '1')])],
        [_conv('a', messages: [_msg('user', 'fresh', id: '2')])],
      );
      expect(merged.single.messages.single.textContent, 'fresh');
    });

    test('conversations absent from the server list are dropped', () {
      final merged = ConversationsNotifier.mergeServerListWithCache(
        [_conv('gone', messages: [_msg('user', 'x', id: '1')])],
        [],
      );
      expect(merged, isEmpty);
    });
  });

  group('mergeServerMessages', () {
    test('regression: empty server snapshot never blanks local history', () {
      final local = _conv('a', messages: [_msg('user', 'keep me', id: '1')]);
      expect(ConversationStorage.mergeServerMessages(local, []), 0);
      expect(local.messages.length, 1);
      expect(local.messages.first.textContent, 'keep me');
    });

    test('server snapshot adds new messages in server order', () {
      final local = _conv('a', messages: [_msg('user', 'u1', id: '1')]);
      final added = ConversationStorage.mergeServerMessages(local, [
        _msg('user', 'u1', id: '1'),
        _msg('assistant', 'a1', id: '2'),
      ]);
      expect(added, 1);
      expect(local.messages.map((m) => m.textContent), ['u1', 'a1']);
    });

    test('local-only messages (optimistic send) are preserved at the tail', () {
      final local = _conv('a', messages: [
        _msg('user', 'persisted', id: '1'),
        _msg('user', 'my optimistic send'),
      ]);
      ConversationStorage.mergeServerMessages(local, [
        _msg('user', 'persisted', id: '1'),
      ]);
      expect(
        local.messages.map((m) => m.textContent),
        ['persisted', 'my optimistic send'],
      );
    });
  });
}
