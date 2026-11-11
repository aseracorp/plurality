import 'package:flutter_test/flutter_test.dart';
import 'package:plurality/utils/textsafe.dart';

void main() {
  // The regression under test: a conversation title starting with an astral
  // emoji (outside the BMP) is TWO UTF-16 code units. `title[0]` returns the
  // lone high surrogate, the web build hands it to ParagraphBuilder.addText and
  // the browser text stack throws
  // "Invalid argument(s): string is not well-formed UTF-16" — inside the
  // conversation sidebar's layout, which kills the whole chat screen.
  group('firstGrapheme', () {
    test('astral emoji yields the whole character, not a lone surrogate', () {
      const title = '💾 Apply pve01 memory fix (zram + OOM protection)';
      final letter = firstGrapheme(title);
      expect(letter, '💾');
      expect(isWellFormedUtf16(letter), isTrue,
          reason: 'avatar letter must be well-formed UTF-16');
      // The bug, for the record: code-unit indexing splits the pair.
      expect(isWellFormedUtf16(title[0]), isFalse);
      expect(title[0], '\uD83D'); // the value that crashed addText
    });

    test('empty string is empty, not an exception', () {
      expect(firstGrapheme(''), '');
    });

    test('plain titles keep their capital letter', () {
      expect(firstGrapheme('Hello world'), 'H');
      expect(firstGrapheme('äpfel'), 'ä');
      expect(firstGrapheme('Ünïcödé'), 'Ü');
      expect(firstGrapheme('日本語'), '日');
    });

    test('combined sequences survive whole', () {
      const flag = '🇨🇭 Chat'; // regional indicators = one grapheme cluster
      expect(firstGrapheme(flag), '🇨🇭');
      expect(isWellFormedUtf16(firstGrapheme(flag)), isTrue);
      const family = '👨‍👩‍👧‍👦 meeting'; // ZWJ sequence
      expect(firstGrapheme(family), '👨‍👩‍👧‍👦');
      expect(isWellFormedUtf16(firstGrapheme(family)), isTrue);
      const skinned = '👋🏽 there'; // skin-tone modifier
      expect(firstGrapheme(skinned), '👋🏽');
      expect(isWellFormedUtf16(firstGrapheme(skinned)), isTrue);
    });

    test('toUpperCase of the result stays well-formed', () {
      expect(firstGrapheme('über conversation').toUpperCase(), 'Ü');
      expect(firstGrapheme('ß test').toUpperCase(), 'ß');
      expect(firstGrapheme('💾 fix').toUpperCase(), '💾');
    });
  });

  group('avatarLetter', () {
    test('emoji title renders the emoji, never a lone surrogate', () {
      expect(avatarLetter('💾 Apply pve01 memory fix'), '💾');
      expect(isWellFormedUtf16(avatarLetter('💾 Apply pve01 memory fix')), isTrue);
    });

    test('empty title falls back to ? like the old isNotEmpty guard', () {
      expect(avatarLetter(''), '?');
    });

    test('ascii and accented titles are uppercased whole', () {
      expect(avatarLetter('hello world'), 'H');
      expect(avatarLetter('über'), 'Ü');
      expect(avatarLetter('ß chat'), 'ß');
      expect(avatarLetter('日本語'), '日');
      expect(avatarLetter('🇨🇭 flags'), '🇨🇭');
      expect(isWellFormedUtf16(avatarLetter('🇨🇭 flags')), isTrue);
    });
  });

  group('headGraphemes', () {
    test('cutting an astral title can never split the emoji', () {
      const s = '💾 Apply pve01 memory fix';
      // substring(0, 1) — the old shape — is a lone surrogate.
      expect(isWellFormedUtf16(s.substring(0, 1)), isFalse);
      for (var n = 0; n <= s.length + 2; n++) {
        expect(isWellFormedUtf16(headGraphemes(s, n)), isTrue,
            reason: 'headGraphemes($n) must be well-formed UTF-16');
      }
      expect(headGraphemes(s, 0), '');
      expect(headGraphemes(s, -1), '');
      expect(headGraphemes(s, 1), '💾');
      expect(headGraphemes(s, 2), '💾 ');
      expect(headGraphemes(s, 7), '💾 Apply');
    });

    test('oversized n returns the string unchanged', () {
      const s = '💾 short';
      expect(headGraphemes(s, 999), s);
      expect(headGraphemes('', 5), '');
    });

    test('truncating a ZWJ sequence keeps it atomic', () {
      const s = '👨‍👩‍👧‍👦 and friends';
      expect(headGraphemes(s, 1), '👨‍👩‍👧‍👦');
      expect(isWellFormedUtf16(headGraphemes(s, 2)), isTrue);
    });
  });

  group('tailGraphemes', () {
    test('tail cut is rune-safe too', () {
      const s = 'apply memory fix 💾';
      for (var n = 0; n <= s.length + 2; n++) {
        expect(isWellFormedUtf16(tailGraphemes(s, n)), isTrue,
            reason: 'tailGraphemes($n) must be well-formed UTF-16');
      }
      expect(tailGraphemes(s, 1), '💾');
      expect(tailGraphemes(s, 4), 'ix 💾');
      expect(tailGraphemes(s, 5), 'fix 💾');
      expect(tailGraphemes(s, 999), s);
      expect(tailGraphemes(s, 0), '');
    });
  });

  group('ellipsize', () {
    test('appends the suffix only when it actually cut', () {
      expect(ellipsize('💾 Apply pve01 memory fix', 7), '💾 Apply...');
      expect(ellipsize('short', 100), 'short');
      expect(ellipsize('💾', 1), '💾');
      expect(ellipsize('abcdef', 3, suffix: '…'), 'abc…');
      expect(isWellFormedUtf16(ellipsize('💾💾💾', 2)), isTrue);
    });
  });

  group('isWellFormedUtf16', () {
    test('detects the lone surrogates index slicing produces', () {
      const s = '💾';
      expect(isWellFormedUtf16(s), isTrue);
      expect(isWellFormedUtf16(s.substring(0, 1)), isFalse); // high alone
      expect(isWellFormedUtf16(s.substring(1)), isFalse); // low alone
      expect(isWellFormedUtf16('plain ascii'), isTrue);
      expect(isWellFormedUtf16(''), isTrue);
      expect(isWellFormedUtf16('\uD83D\uDE00'), isTrue);
      expect(isWellFormedUtf16('\uDBFF\uDFFF'), isTrue);
    });
  });

  test('every helper is well-formed on a stress mix', () {
    const s =
        '💾Äpfel👨‍👩‍👧‍👦🇨🇭日本語 café ü ẍ́ 0123456789 💾💾💾 trailing text';
    for (var n = 0; n <= s.length + 1; n++) {
      expect(isWellFormedUtf16(headGraphemes(s, n)), isTrue,
          reason: 'headGraphemes($n)');
      expect(isWellFormedUtf16(tailGraphemes(s, n)), isTrue,
          reason: 'tailGraphemes($n)');
      expect(isWellFormedUtf16(ellipsize(s, n)), isTrue,
          reason: 'ellipsize($n)');
    }
    expect(isWellFormedUtf16(firstGrapheme(s)), isTrue);
  });
}
