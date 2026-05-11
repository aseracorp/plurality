import 'dart:convert';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:http/http.dart' as http;
import '../auth/auth-service.dart';
import '../utils/types.dart';

class CronJob {
  final String id;
  final String schedule;
  final String prompt;
  final String presetId;
  final bool enabled;
  final DateTime? createdAt;

  CronJob({
    required this.id,
    required this.schedule,
    required this.prompt,
    required this.presetId,
    required this.enabled,
    this.createdAt,
  });

  factory CronJob.fromJson(Map<String, dynamic> json) {
    return CronJob(
      id: json['id'] ?? '',
      schedule: json['schedule'] ?? '',
      prompt: json['prompt'] ?? '',
      presetId: json['preset_id'] ?? '',
      enabled: json['enabled'] ?? true,
      createdAt:
          json['created_at'] != null
              ? DateTime.tryParse(json['created_at'])
              : null,
    );
  }
}

class CronService {
  static final CronService _instance = CronService._internal();
  final AuthService _authService = AuthService();
  String get _baseUrl => AuthService.baseUrl;

  factory CronService() => _instance;
  CronService._internal();

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

  Future<List<CronJob>> list() async {
    final res = await http.get(
      Uri.parse('$_baseUrl/crons'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to fetch crons: ${res.reasonPhrase}',
        statusCode: res.statusCode,
      );
    }
    final decoded = jsonDecode(utf8.decode(res.bodyBytes));
    if (decoded == null) return [];
    return (decoded as List)
        .map((j) => CronJob.fromJson(j as Map<String, dynamic>))
        .toList();
  }

  Future<CronJob> create({
    required String schedule,
    required String prompt,
    String? presetId,
  }) async {
    final res = await http.post(
      Uri.parse('$_baseUrl/crons'),
      headers: await _authHeaders(),
      body: jsonEncode({
        'schedule': schedule,
        'prompt': prompt,
        if (presetId != null) 'preset_id': presetId,
      }),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to create cron: ${utf8.decode(res.bodyBytes)}',
        statusCode: res.statusCode,
      );
    }
    return CronJob.fromJson(jsonDecode(utf8.decode(res.bodyBytes)));
  }

  Future<CronJob> update(
    String id, {
    String? schedule,
    String? prompt,
    String? presetId,
    bool? enabled,
  }) async {
    final body = <String, dynamic>{};
    if (schedule != null) body['schedule'] = schedule;
    if (prompt != null) body['prompt'] = prompt;
    if (presetId != null) body['preset_id'] = presetId;
    if (enabled != null) body['enabled'] = enabled;

    final res = await http.put(
      Uri.parse('$_baseUrl/crons/$id'),
      headers: await _authHeaders(),
      body: jsonEncode(body),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to update cron: ${utf8.decode(res.bodyBytes)}',
        statusCode: res.statusCode,
      );
    }
    return CronJob.fromJson(jsonDecode(utf8.decode(res.bodyBytes)));
  }

  Future<void> delete(String id) async {
    final res = await http.delete(
      Uri.parse('$_baseUrl/crons/$id'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to delete cron: ${res.reasonPhrase}',
        statusCode: res.statusCode,
      );
    }
  }

  Future<void> run(String id) async {
    final res = await http.post(
      Uri.parse('$_baseUrl/crons/$id/run'),
      headers: await _authHeaders(),
    );
    if (res.statusCode < 200 || res.statusCode >= 300) {
      throw APIException(
        'Failed to run cron: ${res.reasonPhrase}',
        statusCode: res.statusCode,
      );
    }
  }
}

class CronsNotifier extends StateNotifier<List<CronJob>> {
  final _service = CronService();

  CronsNotifier() : super([]) {
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

  Future<void> add({
    required String schedule,
    required String prompt,
    String? presetId,
  }) async {
    final job = await _service.create(
      schedule: schedule,
      prompt: prompt,
      presetId: presetId,
    );
    state = [...state, job];
  }

  Future<void> update(
    String id, {
    String? schedule,
    String? prompt,
    String? presetId,
    bool? enabled,
  }) async {
    final job = await _service.update(
      id,
      schedule: schedule,
      prompt: prompt,
      presetId: presetId,
      enabled: enabled,
    );
    state = [
      for (final j in state)
        if (j.id == id) job else j,
    ];
  }

  Future<void> remove(String id) async {
    await _service.delete(id);
    state = state.where((j) => j.id != id).toList();
  }

  Future<void> runNow(String id) async {
    await _service.run(id);
  }
}

final cronsProvider = StateNotifierProvider<CronsNotifier, List<CronJob>>(
  (ref) => CronsNotifier(),
);
