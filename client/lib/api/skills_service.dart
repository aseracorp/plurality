import 'dart:convert';
import 'dart:io';
import 'package:path_provider/path_provider.dart';
import 'package:path/path.dart' as path;

class SkillInfo {
  final String name;
  final String description;
  final String path;

  SkillInfo({required this.name, required this.description, required this.path});
}

class SkillsService {
  static final SkillsService _instance = SkillsService._internal();
  static SkillsService get instance => _instance;

  List<SkillInfo> _skills = [];

  factory SkillsService() {
    return _instance;
  }

  SkillsService._internal();

  Future<Directory> getSkillsPath() async {
    final appDocumentDirectory = await getApplicationSupportDirectory();
    return Directory(path.join(appDocumentDirectory.path, 'skills'));
  }

  Future<void> initSkills() async {
    if (Platform.isWindows || Platform.isLinux || Platform.isMacOS) {
      _skills = [];

      final skillsDir = await getSkillsPath();

      if (!await skillsDir.exists()) {
        await skillsDir.create(recursive: true);
        return;
      }

      await for (final entity in skillsDir.list()) {
        if (entity is Directory) {
          final skillName = path.basename(entity.path);
          final skillFile = File(path.join(entity.path, 'SKILL.md'));

          if (await skillFile.exists()) {
            _skills.add(SkillInfo(
              name: skillName,
              description: await _readMetaDescription(entity.path),
              path: entity.path,
            ));
          }
        }
      }
    }
  }

  Future<String> _readMetaDescription(String skillPath) async {
    try {
      final meta = File(path.join(skillPath, 'meta.json'));
      if (!await meta.exists()) return '';
      final content = await meta.readAsString();
      final decoded = jsonDecode(content);
      if (decoded is Map && decoded['description'] is String) {
        return (decoded['description'] as String).trim();
      }
    } catch (_) {}
    return '';
  }

  List<SkillInfo> getSkillList() {
    return List<SkillInfo>.from(_skills);
  }

  List<String> getSkillNames() {
    return _skills.map((s) => s.name).toList();
  }

  Map<String, dynamic> getToolDefinition() {
    return {
      'name': 'retrieve_skill',
      'description': 'Retrieve a skill\'s instructions from the local skills folder.',
      'parameters': {
        'type': 'object',
        'properties': {
          'skill_name': {
            'type': 'string',
            'description': 'Name of the skill to retrieve',
          },
          'file_name': {
            'type': 'string',
            'description':
                'Optional file to read from the skill folder (defaults to SKILL.md)',
          },
        },
        'required': ['skill_name'],
      },
    };
  }

  Future<String> executeRetrieveSkill(
    String skillName,
    String? fileName,
  ) async {
    final targetFile = fileName ?? 'SKILL.md';

    // Sanitize inputs to prevent directory traversal
    if (skillName.contains('..') || targetFile.contains('..')) {
      return 'Error: Invalid path — ".." is not allowed.';
    }
    if (skillName.contains('/') ||
        skillName.contains('\\') ||
        targetFile.contains('/') ||
        targetFile.contains('\\')) {
      return 'Error: Invalid path — slashes are not allowed in skill_name or file_name.';
    }

    final skillsDir = await getSkillsPath();
    final filePath = path.join(skillsDir.path, skillName, targetFile);

    // Double-check resolved path is within skills directory
    final resolved = path.canonicalize(filePath);
    final skillsResolved = path.canonicalize(skillsDir.path);
    if (!resolved.startsWith(skillsResolved)) {
      return 'Error: Invalid path — resolved path is outside the skills directory.';
    }

    final file = File(filePath);
    if (!await file.exists()) {
      return 'Error: File not found — $skillName/$targetFile does not exist.';
    }

    try {
      var content = await file.readAsString();
      // Cap at ~50KB
      if (content.length > 50000) {
        content = content.substring(0, 50000) +
            '\n\n[Content truncated — file exceeds 50KB limit]';
      }
      return content;
    } catch (e) {
      return 'Error: Failed to read file — $e';
    }
  }
}
