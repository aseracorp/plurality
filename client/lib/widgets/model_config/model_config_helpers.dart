import 'package:flutter/material.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/models_service.dart';
import 'package:plurality/api/skills_service.dart';
import 'package:plurality/utils/types.dart';

/// Convert map value ("true"/"ask"/null) to toggle state ("on"/"ask"/"off").
String mapValueToToggle(String? mapValue) {
  if (mapValue == null) return 'off';
  if (mapValue == 'ask') return 'ask';
  return 'on';
}

MaterialColor colorFromName(String name) {
  switch (name) {
    case 'green':
      return Colors.green;
    case 'blue':
      return Colors.blue;
    case 'purple':
      return Colors.purple;
    case 'red':
      return Colors.red;
    case 'orange':
      return Colors.orange;
    default:
      return Colors.grey;
  }
}

/// A preset's per-field name is optional. An empty/missing name acts as a
/// wildcard against the current selection.
bool presetNameMatches(String? presetName, String currentName) =>
    presetName == null || presetName.isEmpty || presetName == currentName;

/// Merge a preset onto the current ModelSelected.
/// - A null per-field model means "preset doesn't touch this field" -> keep current.
/// - An empty/missing name on a per-field model means "keep currently selected model".
/// - Tools are additive: per-key entries from the preset override the current
///   value for that key, but other current entries are preserved. A preset
///   value of "false" subtracts the key from the resulting set.
ModelSelected mergePresetOnto(ModelSelected preset, ModelSelected current) {
  return ModelSelected(
    text: mergeModel(preset.text, current.text),
    vision: mergeModel(preset.vision, current.vision),
    imageGen: mergeModel(preset.imageGen, current.imageGen),
    audioGen: mergeModel(preset.audioGen, current.audioGen),
    voiceGen: mergeModel(preset.voiceGen, current.voiceGen),
    audioTranscribe: mergeModel(preset.audioTranscribe, current.audioTranscribe),
    videoGen: mergeModel(preset.videoGen, current.videoGen),
    videoVision: mergeModel(preset.videoVision, current.videoVision),
    code: mergeModel(preset.code, current.code),
  );
}

Model? mergeModel(Model? presetModel, Model? currentModel) {
  if (presetModel == null) return currentModel;
  final name = presetModel.name.isNotEmpty
      ? presetModel.name
      : (currentModel?.name ?? '');
  final mergedTools = <String, String>{...?currentModel?.tools};
  presetModel.tools.forEach((key, value) {
    if (value == 'false') {
      mergedTools.remove(key);
    } else {
      mergedTools[key] = value;
    }
  });
  return Model(
    name: name,
    params: presetModel.params ?? currentModel?.params,
    tools: mergedTools,
  );
}

/// Build the functions list (server functions + local MCP tools), grouped by
/// bundle parent. Each entry has shape:
///   { key, label, description, enabled: 'on'|'ask'|'off',
///     tools?: List<String>, source: 'server'|null }
List<Map<String, dynamic>> buildFunctions(ModelsData data) {
  List<Map<String, dynamic>> finalList = [];

  for (var func in data.functions) {
    final parent = func.parent;
    if (parent != null && parent.isNotEmpty) {
      final existing = finalList.where((e) => e['key'] == parent);
      if (existing.isNotEmpty) {
        existing.first['tools'].add(func.key);
      } else {
        final bundle = data.bundles[parent];
        finalList.add({
          'key': parent,
          'label': bundle?.label ?? parent,
          'description': bundle?.description ?? '',
          'enabled': func.defaultState,
          'tools': [func.key],
          'source': 'server',
        });
      }
    } else {
      finalList.add({
        'key': func.key,
        'label': func.label,
        'description': func.description,
        'enabled': func.defaultState,
        'source': 'server',
      });
    }
  }

  final clientSide = MCPService().getToolList();
  if (clientSide.isEmpty) return finalList;

  for (var i = 0; i < clientSide.length; i++) {
    final tool = clientSide[i];
    final serverName = MCPService().getToolServerName(tool['name']);
    if (serverName == null || serverName.isEmpty) continue;

    var description = tool['description'] ?? 'No description available';
    if (description.length > 100) {
      description = description.substring(0, 100) + '...';
    }
    if (description.contains('\n')) {
      description = description.split('\n')[0];
    }

    if (finalList.any((element) => element['key'] == serverName)) {
      finalList
          .firstWhere((element) => element['key'] == serverName)['tools']
          .add(tool['name']);
    } else {
      finalList.add({
        'key': serverName,
        'label': serverName,
        'description': description,
        'enabled': 'on',
        'tools': [tool['name']],
      });
    }
  }

  return finalList;
}

/// Build the skill items list (local skills first, server skills second).
/// Entry shape: { key, label, description, enabled, source: 'local'|'server' }
List<Map<String, dynamic>> initSkillItems([ModelsData? data]) {
  final clientSkills = SkillsService().getSkillList();
  final seen = <String>{};
  final result = <Map<String, dynamic>>[];

  for (final skill in clientSkills) {
    if (seen.add(skill.name)) {
      result.add({
        'key': skill.name,
        'label': skill.name,
        'description': skill.description,
        'enabled': 'on',
        'source': 'local',
      });
    }
  }

  if (data != null) {
    for (final skill in data.skills) {
      if (seen.add(skill.name)) {
        result.add({
          'key': skill.name,
          'label': skill.name,
          'description': skill.description,
          'enabled': skill.defaultState,
          'source': 'server',
        });
      }
    }
  }

  return result;
}

/// Apply the current functions/skills toggle states to a tools map suitable
/// for `Model.tools`. Also injects `retrieve_skill` / `retrieve_server_skill`
/// based on whether any local/server skills are enabled.
Map<String, String> toolsMapFromFunctionsAndSkills(
  List<Map<String, dynamic>> functions,
  List<Map<String, dynamic>> skills,
) {
  final Map<String, String> toolsMap = {};

  for (final function in functions) {
    final mode = function['enabled'] as String;
    if (mode == 'off') continue;
    final mapValue = mode == 'ask' ? 'ask' : 'true';
    final tools = function['tools'];
    if (tools != null && tools is List) {
      for (final tool in tools) {
        toolsMap[tool as String] = mapValue;
      }
    } else {
      toolsMap[function['key'] as String] = mapValue;
    }
  }

  bool hasEnabledLocalSkills = false;
  bool hasEnabledServerSkills = false;
  for (final skill in skills) {
    final mode = skill['enabled'] as String;
    if (mode == 'off') continue;
    final mapValue = mode == 'ask' ? 'ask' : 'true';
    toolsMap[skill['key'] as String] = mapValue;
    if (skill['source'] == 'server') {
      hasEnabledServerSkills = true;
    } else {
      hasEnabledLocalSkills = true;
    }
  }
  if (hasEnabledLocalSkills) {
    toolsMap['retrieve_skill'] = 'true';
  }
  if (hasEnabledServerSkills) {
    toolsMap['retrieve_server_skill'] = 'true';
  }

  return toolsMap;
}

/// Apply a tools map back onto a freshly-built functions list so the UI
/// reflects the user's prior selection. Mutates `functions` in place.
void applyToolsMapToFunctions(
  List<Map<String, dynamic>> functions,
  Map<String, String> toolsMap,
) {
  for (var function in functions) {
    if (function['tools'] == null) {
      final mode = toolsMap[function['key']];
      function['enabled'] = mapValueToToggle(mode);
    } else {
      bool allEnabled = true;
      String bundleMode = 'off';
      for (var tool in function['tools']) {
        final mode = toolsMap[tool];
        if (mode != null) {
          bundleMode = mapValueToToggle(mode);
        } else {
          allEnabled = false;
          break;
        }
      }
      function['enabled'] = allEnabled ? bundleMode : 'off';
    }
  }
}

void applyToolsMapToSkills(
  List<Map<String, dynamic>> skills,
  Map<String, String> toolsMap,
) {
  for (var skill in skills) {
    final mode = toolsMap[skill['key']];
    skill['enabled'] = mapValueToToggle(mode);
  }
}
