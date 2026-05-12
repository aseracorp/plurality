import 'package:flutter_riverpod/flutter_riverpod.dart';

final pendingConversationIdProvider = StateProvider<String?>((ref) => null);

String? parseConversationDeepLink(Uri uri) {
  if (uri.scheme != 'plurality') return null;
  if (uri.host != 'conversation') return null;
  final segments = uri.pathSegments.where((s) => s.isNotEmpty).toList();
  if (segments.isEmpty) return null;
  return segments.first;
}
