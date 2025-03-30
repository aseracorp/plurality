String sanitizeMessages(String message) {
  return message.replaceAll(
    RegExp(
      r'<hidden>((.|\n)*)</hidden>',
      multiLine: true,
      caseSensitive: false,
    ),
    '',
  );
}
