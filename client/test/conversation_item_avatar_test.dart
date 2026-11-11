// Widget-level regression test for the conversation-list avatar crash.
//
// The conversation list renders each conversation's avatar as
// `title[0].toUpperCase()`. For a title starting with an astral-plane
// character (emoji), Dart string indexing returns a single UTF-16 code unit —
// half a surrogate pair — and the canvas text renderer throws while painting
// it: "RangeError (value): Input must be a valid UTF-32 Code Unit: 55357".
//
// `avatarLetter` (lib/utils/textsafe.dart) takes the first *grapheme cluster*
// instead. These tests pump the real ConversationItem so a regression at the
// call site — not just in the helper — fails loudly.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:plurality/chat/conversation-list/conversation-item.dart';
import 'package:plurality/utils/types.dart';

Conversation _conv(String title) => Conversation(
      id: 'c-$title',
      title: title,
      lastMessageAt: DateTime(2026, 1, 1),
      modelSelected: ModelSelected(),
      messages: [],
    );

Future<void> _pump(WidgetTester tester, String title) async {
  await tester.pumpWidget(
    ProviderScope(
      child: MaterialApp(
        home: Scaffold(
          body: Consumer(
            builder: (context, ref, _) => ConversationItem(
              conversation: _conv(title),
              isSelected: false,
              onSelect: (_) {},
              ref: ref,
              onTitleUpdate: () {},
              onDelete: () {},
            ),
          ),
        ),
      ),
    ),
  );
  await tester.pumpAndSettle();
}

/// The string rendered inside the avatar's CircleAvatar.
String _avatarText(WidgetTester tester, String expected) {
  final matches = find.descendant(
    of: find.byType(CircleAvatar),
    matching: find.text(expected),
  );
  expect(matches, findsOneWidget, reason: 'no CircleAvatar child Text "$expected"');
  return tester.widget<Text>(matches).data!;
}

/// Assert every UTF-16 code unit of [s] is a standalone scalar value, i.e. no
/// surrogate is left unpaired. Dart strings are UTF-16, so `s[0]` of an emoji
/// yields exactly the unpaired half that crashed the renderer.
void _expectNoLoneSurrogate(String s, String label) {
  for (var i = 0; i < s.length; i++) {
    final cu = s.codeUnitAt(i);
    final isHigh = cu >= 0xD800 && cu <= 0xDBFF;
    final isLow = cu >= 0xDC00 && cu <= 0xDFFF;
    if (isHigh) {
      final next = i + 1 < s.length ? s.codeUnitAt(i + 1) : -1;
      expect(
        next >= 0xDC00 && next <= 0xDFFF,
        isTrue,
        reason: '$label: lone high surrogate at index $i (U+$cu)',
      );
      i++; // the pair is consumed
    } else {
      expect(
        isLow,
        isFalse,
        reason: '$label: lone low surrogate at index $i (U+$cu)',
      );
    }
  }
}

void main() {
  group('ConversationItem avatar (astral-plane titles)', () {
    testWidgets('emoji title paints without a RangeError', (tester) async {
      // Pre-fix this throws while laying out/painting the avatar Text.
      await _pump(tester, '\u{1F4BE} Floppy disk project');
      expect(tester.takeException(), isNull);
    });

    testWidgets('emoji avatar renders the whole emoji, not a lone surrogate',
        (tester) async {
      await _pump(tester, '\u{1F4BE} Floppy disk project');
      final rendered = _avatarText(tester, '\u{1F4BE}');
      expect(rendered, '\u{1F4BE}');
      _expectNoLoneSurrogate(rendered, 'avatar text');
      // Sanity check on the fixture itself: the emoji starts with a high
      // surrogate, so naive `[0]` indexing (the old code) handed the renderer
      // an unpaired code unit.
      expect(rendered.codeUnitAt(0) >= 0xD800, isTrue);
    });

    testWidgets('astral title with no following text', (tester) async {
      await _pump(tester, '\u{1F680}');
      expect(tester.takeException(), isNull);
      // Title == emoji here, so the avatar Text and the title Text are the
      // same string: scope to the avatar.
      final avatarText = tester
          .widgetList<Text>(
              find.descendant(
                of: find.byType(CircleAvatar),
                matching: find.text('\u{1F680}'),
              ))
          .map((t) => t.data)
          .toList();
      expect(avatarText, ['\u{1F680}']);
    });

    testWidgets('flag sequence keeps one grapheme', (tester) async {
      // Regional indicators are 2 code units each; the cluster stays whole.
      await _pump(tester, '\u{1F1E6}\u{1F1E8} Roadtrip');
      expect(tester.takeException(), isNull);
      final rendered = _avatarText(tester, '\u{1F1E6}\u{1F1E8}');
      _expectNoLoneSurrogate(rendered, 'flag avatar');
    });

    testWidgets('plain ascii title keeps the old capitalised letter',
        (tester) async {
      await _pump(tester, 'hello world');
      expect(tester.takeException(), isNull);
      expect(find.descendant(of: find.byType(CircleAvatar), matching: find.text('H')),
          findsOneWidget);
    });

    testWidgets('accented lowercase title uppercases whole', (tester) async {
      await _pump(tester, '\u00E9clair');
      expect(tester.takeException(), isNull);
      expect(
          find.descendant(
              of: find.byType(CircleAvatar), matching: find.text('\u00C9')),
          findsOneWidget);
    });

    testWidgets('empty title falls back to ?', (tester) async {
      await _pump(tester, '');
      expect(tester.takeException(), isNull);
      expect(
          find.descendant(of: find.byType(CircleAvatar), matching: find.text('?')),
          findsOneWidget);
    });
  });
}
