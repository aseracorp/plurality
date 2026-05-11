import 'dart:convert';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:http/http.dart' as http;
import '../auth/auth-service.dart';
import '../utils/types.dart';

/// One webhook definition as the client sees it. The plaintext token is
/// NEVER returned by the list endpoint — only by create/rotate, where it
/// lives on [WebhookCreateResult].
class Webhook {
  final String id;
  final String prompt;
  final String presetId;
  final bool enabled;
  final DateTime createdAt;
  final DateTime? lastTriggeredAt;
  final String conversationId;

  Webhook({
    required this.id,
    required this.prompt,
    required this.presetId,
    required this.enabled,
    required this.createdAt,
    this.lastTriggeredAt,
    this.conversationId = '',
  });

  factory Webhook.fromJson(Map<String, dynamic> json) {
    return Webhook(
      id: json['id'] ?? '',
      prompt: json['prompt'] ?? '',
      presetId: json['preset_id'] ?? '',
      enabled: json['enabled'] ?? true,
      createdAt:
          DateTime.tryParse(json['created_at'] ?? '') ?? DateTime.now(),
      lastTriggeredAt: json['last_triggered_at'] != null
          ? DateTime.tryParse(json['last_triggered_at'])
          : null,
      conversationId: json['conversation_id'] ?? '',
    );
  }
}

/// Result of a create or rotate call. The [url] and [token] are visible
/// EXACTLY ONCE — the user (or LLM) must copy them now; the server can't
/// recover the plaintext.
class WebhookCreateResult {
  final Webhook webhook;
  final String url;
  final String token;

  WebhookCreateResult({
    required this.webhook,
    required this.url,
    required this.token,
  });

  factory WebhookCreateResult.fromJson(Map<String, dynamic> json) {
    return WebhookCreateResult(
      webhook: Webhook.fromJson(json),
      url: json['url'] ?? '',
      token: json['token'] ?? '',
    );
  }
}

class WebhookService {
  static final WebhookService _instance = WebhookService._internal();
  final AuthService _authService = AuthService();
  String get _baseUrl => AuthService.baseUrl;

  factory WebhookService() => _instance;
  WebhookService._internal();

  Future<Map<String, String>> _authHeaders() async {
    final token = await _authService.getCurrentUserToken();
    if (token == null) {
      throw Exception('User not authenticated');
    }
    return {
      'Content-Type': 'application/json',
      'Authorization': 'Bearer $token',
    };
  }

  Future<List<Webhook>> list() async {
    final res = await http.get(
      Uri.parse('$_baseUrl/webhooks'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to fetch webhooks: ${res.reasonPhrase}',
        statusCode: res.statusCode,
      );
    }
    final decoded = jsonDecode(utf8.decode(res.bodyBytes));
    if (decoded == null) return [];
    return (decoded as List)
        .map((j) => Webhook.fromJson(j as Map<String, dynamic>))
        .toList();
  }

  Future<WebhookCreateResult> create({
    required String prompt,
    String? presetId,
    String? conversationId,
  }) async {
    final res = await http.post(
      Uri.parse('$_baseUrl/webhooks'),
      headers: await _authHeaders(),
      body: jsonEncode({
        'prompt': prompt,
        if (presetId != null) 'preset_id': presetId,
        if (conversationId != null) 'conversation_id': conversationId,
      }),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to create webhook: ${utf8.decode(res.bodyBytes)}',
        statusCode: res.statusCode,
      );
    }
    return WebhookCreateResult.fromJson(jsonDecode(utf8.decode(res.bodyBytes)));
  }

  Future<Webhook> update(
    String id, {
    String? prompt,
    String? presetId,
    bool? enabled,
    String? conversationId,
  }) async {
    final body = <String, dynamic>{};
    if (prompt != null) body['prompt'] = prompt;
    if (presetId != null) body['preset_id'] = presetId;
    if (enabled != null) body['enabled'] = enabled;
    if (conversationId != null) body['conversation_id'] = conversationId;

    final res = await http.put(
      Uri.parse('$_baseUrl/webhooks/$id'),
      headers: await _authHeaders(),
      body: jsonEncode(body),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to update webhook: ${utf8.decode(res.bodyBytes)}',
        statusCode: res.statusCode,
      );
    }
    return Webhook.fromJson(jsonDecode(utf8.decode(res.bodyBytes)));
  }

  Future<void> delete(String id) async {
    final res = await http.delete(
      Uri.parse('$_baseUrl/webhooks/$id'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to delete webhook: ${res.reasonPhrase}',
        statusCode: res.statusCode,
      );
    }
  }

  Future<WebhookCreateResult> rotateToken(String id) async {
    final res = await http.post(
      Uri.parse('$_baseUrl/webhooks/$id/rotate'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to rotate token: ${utf8.decode(res.bodyBytes)}',
        statusCode: res.statusCode,
      );
    }
    return WebhookCreateResult.fromJson(jsonDecode(utf8.decode(res.bodyBytes)));
  }
}

class WebhooksNotifier extends StateNotifier<List<Webhook>> {
  final _service = WebhookService();

  WebhooksNotifier() : super([]) {
    refresh();
  }

  Future<void> refresh() async {
    try {
      final list = await _service.list();
      state = list;
    } catch (_) {
      // keep stale state on error
    }
  }

  /// Returns the full create result so the caller can show the one-shot
  /// URL/token. State is updated optimistically.
  Future<WebhookCreateResult> add({
    required String prompt,
    String? presetId,
    String? conversationId,
  }) async {
    final result = await _service.create(
      prompt: prompt,
      presetId: presetId,
      conversationId: conversationId,
    );
    state = [...state, result.webhook];
    return result;
  }

  Future<void> update(
    String id, {
    String? prompt,
    String? presetId,
    bool? enabled,
    String? conversationId,
  }) async {
    final hook = await _service.update(
      id,
      prompt: prompt,
      presetId: presetId,
      enabled: enabled,
      conversationId: conversationId,
    );
    state = [
      for (final w in state)
        if (w.id == id) hook else w,
    ];
  }

  Future<void> remove(String id) async {
    await _service.delete(id);
    state = state.where((w) => w.id != id).toList();
  }

  Future<WebhookCreateResult> rotateToken(String id) async {
    final result = await _service.rotateToken(id);
    state = [
      for (final w in state)
        if (w.id == id) result.webhook else w,
    ];
    return result;
  }
}

final webhooksProvider =
    StateNotifierProvider<WebhooksNotifier, List<Webhook>>(
  (ref) => WebhooksNotifier(),
);
