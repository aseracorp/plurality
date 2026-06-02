import 'dart:convert';
import 'package:http/http.dart' as http;
import '../auth/auth-service.dart';
import '../utils/types.dart';
import './api.dart';

class ModelInfo {
  final String id;
  final bool text;
  final bool vision;
  final bool imageGen;
  final bool imageEdit;
  final bool audio;

  ModelInfo({
    required this.id,
    this.text = false,
    this.vision = false,
    this.imageGen = false,
    this.imageEdit = false,
    this.audio = false,
  });

  factory ModelInfo.fromJson(Map<String, dynamic> json) => ModelInfo(
        id: json['id'] as String? ?? '',
        text: json['text'] == true,
        vision: json['vision'] == true,
        imageGen: json['image_gen'] == true,
        imageEdit: json['image_edit'] == true,
        audio: json['audio'] == true,
      );
}

class PresetConfig {
  final String name;
  final String label;
  final String pricing;
  final String color;
  final int order;
  final ModelSelected models;

  PresetConfig({
    required this.name,
    required this.label,
    required this.pricing,
    required this.color,
    required this.order,
    required this.models,
  });

  factory PresetConfig.fromJson(Map<String, dynamic> json) => PresetConfig(
        name: json['name'] as String? ?? '',
        label: json['label'] as String? ?? '',
        pricing: json['pricing'] as String? ?? '',
        color: json['color'] as String? ?? '',
        order: json['order'] as int? ?? 0,
        models: ModelSelected.fromJson(
          (json['models'] as Map?)?.cast<String, dynamic>() ?? const {},
        ),
      );
}

class FunctionDef {
  final String key;
  final String label;
  final String description;
  final String defaultState;
  final String? parent;

  FunctionDef({
    required this.key,
    required this.label,
    required this.description,
    required this.defaultState,
    this.parent,
  });

  factory FunctionDef.fromJson(Map<String, dynamic> json) => FunctionDef(
        key: json['key'] as String? ?? '',
        label: json['label'] as String? ?? json['key'] as String? ?? '',
        description: json['description'] as String? ?? '',
        defaultState: json['default'] as String? ?? 'on',
        parent: json['parent'] as String?,
      );
}

class FunctionBundle {
  final String key;
  final String label;
  final String description;

  FunctionBundle({required this.key, required this.label, required this.description});

  factory FunctionBundle.fromJson(Map<String, dynamic> json) => FunctionBundle(
        key: json['key'] as String? ?? '',
        label: json['label'] as String? ?? json['key'] as String? ?? '',
        description: json['description'] as String? ?? '',
      );
}

class ServerSkillInfo {
  final String name;
  final String description;
  final String defaultState;

  ServerSkillInfo({
    required this.name,
    required this.description,
    required this.defaultState,
  });

  factory ServerSkillInfo.fromJson(Map<String, dynamic> json) => ServerSkillInfo(
        name: json['name'] as String? ?? '',
        description: json['description'] as String? ?? '',
        defaultState: json['default'] as String? ?? 'on',
      );
}

class ModelsData {
  final List<ModelInfo> models;
  final List<PresetConfig> presets;
  final List<FunctionDef> functions;
  final Map<String, FunctionBundle> bundles;
  final List<ServerSkillInfo> skills;

  ModelsData({
    required this.models,
    required this.presets,
    required this.functions,
    required this.bundles,
    this.skills = const [],
  });

  List<String> get textModelIds =>
      models.where((m) => m.text).map((m) => m.id).toList();

  List<String> get visionModelIds =>
      models.where((m) => m.vision).map((m) => m.id).toList();

  List<String> get imageGenModelIds =>
      models.where((m) => m.imageGen).map((m) => m.id).toList();

  List<String> get imageEditModelIds =>
      models.where((m) => m.imageEdit).map((m) => m.id).toList();

  PresetConfig? get fastPreset =>
      presets.where((p) => p.name == 'Fast').firstOrNull;

  factory ModelsData.fromJson(Map<String, dynamic> json) {
    final data = (json['data'] as List? ?? const [])
        .map((e) => ModelInfo.fromJson((e as Map).cast<String, dynamic>()))
        .toList();
    final presets = (json['presets'] as List? ?? const [])
        .map((e) => PresetConfig.fromJson((e as Map).cast<String, dynamic>()))
        .toList();
    final functions = (json['functions'] as List? ?? const [])
        .map((e) => FunctionDef.fromJson((e as Map).cast<String, dynamic>()))
        .toList();
    final bundlesRaw = (json['function_bundles'] as Map?)?.cast<String, dynamic>() ?? const {};
    final bundles = bundlesRaw.map(
      (k, v) => MapEntry(k, FunctionBundle.fromJson((v as Map).cast<String, dynamic>())),
    );
    final skills = (json['skills'] as List? ?? const [])
        .map((e) => ServerSkillInfo.fromJson((e as Map).cast<String, dynamic>()))
        .toList();
    return ModelsData(
      models: data,
      presets: presets,
      functions: functions,
      bundles: bundles,
      skills: skills,
    );
  }
}

class ModelsService {
  static final ModelsService _instance = ModelsService._();
  factory ModelsService() => _instance;
  ModelsService._();

  static const Duration _ttl = Duration(minutes: 10);

  final AuthService _auth = AuthService();

  ModelsData? _cache;
  DateTime? _fetchedAt;
  Future<ModelsData>? _inflight;

  ModelsData? get cached => _cache;

  /// Drop the in-memory cache so the next [get] hits the server again.
  /// Use after mutating server-side config (e.g. shortcut edits).
  void invalidate() {
    _cache = null;
    _fetchedAt = null;
  }

  /// Returns model data. With [forceRefresh] the TTL is bypassed and the
  /// server is always hit (the existing [cached] value is kept until the
  /// fresh data arrives, so readers of [cached] never see null mid-refresh).
  /// If the refresh fails (e.g. the server is offline) but a cached value
  /// exists, that stale value is returned instead of surfacing the error.
  Future<ModelsData> get({bool forceRefresh = false}) {
    final fetchedAt = _fetchedAt;
    if (!forceRefresh && _cache != null && fetchedAt != null &&
        DateTime.now().difference(fetchedAt) < _ttl) {
      return Future.value(_cache!);
    }
    _inflight ??= _fetch().catchError((error) {
      final cached = _cache;
      if (cached != null) return cached;
      throw error;
    }).whenComplete(() => _inflight = null);
    return _inflight!;
  }

  Future<ModelsData> _fetch() async {
    final token = await _auth.getCurrentUserToken();
    if (token == null) {
      throw Exception('Not authenticated');
    }
    final response = await http.get(
      Uri.parse('${ApiService.baseUrl}/models'),
      headers: {'Authorization': 'Bearer $token'},
    );
    if (response.statusCode < 200 || response.statusCode >= 300) {
      throw Exception('Failed to load models: ${response.statusCode}');
    }
    final decoded = jsonDecode(utf8.decode(response.bodyBytes)) as Map<String, dynamic>;
    final data = ModelsData.fromJson(decoded);
    _cache = data;
    _fetchedAt = DateTime.now();
    return data;
  }
}
