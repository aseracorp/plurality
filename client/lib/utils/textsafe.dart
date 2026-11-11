/// Truncation / first-letter helpers that never split a character.
///
/// Dart strings are UTF-16. Indexing (`s[0]`) and slicing (`s.substring(0, n)`)
/// work on UTF-16 *code units*, so a character outside the Basic Multilingual
/// Plane — an emoji like 💾 (U+1F4BE) — is two code units: a high surrogate and
/// a low surrogate. Cutting at index 1 therefore yields a string holding a
/// LONE high surrogate.
///
/// That is not cosmetic. On the web build the string reaches
/// `ParagraphBuilder.addText`, and the browser text stack rejects it with
/// `Invalid argument(s): string is not well-formed UTF-16`. In the conversation
/// sidebar that throws inside `RenderSuperSliverList` layout, so it throws on
/// every frame and the whole chat screen goes dead — one conversation titled
/// with an emoji makes the app unusable.
///
/// Everything here works on extended grapheme clusters, so a cut can never
/// land inside a character, and combined emoji (skin-tone modifiers, ZWJ
/// sequences, flags) survive whole instead of rendering half a glyph.
library;

import 'package:characters/characters.dart';

/// The first grapheme cluster of [s] — the avatar letter.
///
/// Returns '' for an empty string. Never a lone surrogate.
String firstGrapheme(String s) {
  if (s.isEmpty) return '';
  return s.characters.first;
}

/// The letter to render in a conversation avatar: the first grapheme cluster of
/// [title], uppercased, or '?' when there is nothing renderable.
///
/// `title[0].toUpperCase()` — the shape this replaces — returns a lone high
/// surrogate for a title that starts with an astral emoji, and the web build
/// then dies in text layout. Keep the fallback behaviour of the old
/// `isNotEmpty ? … : '?'` guard in one place instead.
String avatarLetter(String title) {
  final g = firstGrapheme(title);
  if (g.isEmpty) return '?';
  return g.toUpperCase();
}

/// [s] limited to at most [n] grapheme clusters, keeping the head.
String headGraphemes(String s, int n) {
  if (n <= 0) return '';
  if (s.characters.length <= n) return s;
  return s.characters.take(n).string;
}

/// [s] limited to at most [n] grapheme clusters, keeping the tail.
String tailGraphemes(String s, int n) {
  if (n <= 0) return '';
  if (s.characters.length <= n) return s;
  return s.characters.takeLast(n).string;
}

/// [s] cut to [n] grapheme clusters with an appended [suffix] when it was cut.
String ellipsize(String s, int n, {String suffix = '...'}) {
  if (n <= 0) return s.isEmpty ? '' : suffix;
  if (s.characters.length <= n) return s;
  return '${headGraphemes(s, n)}$suffix';
}

/// Whether [s] is well-formed UTF-16, i.e. every surrogate has its pair
/// adjacent. The web text stack requires this; test/textsafe_test.dart uses it
/// to assert the defect this library exists to prevent.
bool isWellFormedUtf16(String s) {
  final units = s.codeUnits;
  for (var i = 0; i < units.length; i++) {
    final c = units[i];
    if (c >= 0xD800 && c <= 0xDBFF) {
      // high surrogate: must be followed by a low surrogate
      if (i + 1 >= units.length) return false;
      final next = units[i + 1];
      if (next < 0xDC00 || next > 0xDFFF) return false;
      i++;
    } else if (c >= 0xDC00 && c <= 0xDFFF) {
      // low surrogate with no high surrogate in front of it
      return false;
    }
  }
  return true;
}
