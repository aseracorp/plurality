import 'dart:io' show Platform, Process;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/models_service.dart';
import 'package:plurality/api/skills_service.dart';
import '../../api/balance.dart';
import '../../utils/types.dart';

class ModelSelectionModal extends ConsumerStatefulWidget {
  final Function(ModelSelected) onModelSelected;
  final ModelSelected selectedModel;

  const ModelSelectionModal({
    Key? key,
    required this.onModelSelected,
    required this.selectedModel,
  }) : super(key: key);

  @override
  ConsumerState<ModelSelectionModal> createState() =>
      ModelSelectionModalState();
}

class ModelSelectionModalState extends ConsumerState<ModelSelectionModal>
    with SingleTickerProviderStateMixin {
  late Future<ModelsData> _modelsFuture;

  /// Convert map value ("true"/"ask"/null) to toggle state ("on"/"ask"/"off")
  static String _mapValueToToggle(String? mapValue) {
    if (mapValue == null) return 'off';
    if (mapValue == 'ask') return 'ask';
    return 'on';
  }

  static MaterialColor _colorFromName(String name) {
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

  List<Map<String, dynamic>> _functions = [];
  List<Map<String, dynamic>> _skillItems = [];

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

  String _selectedModel = '';
  String _selectedVisionModel = '';
  String _selectedImageGenModel = '';

  late TabController _tabController;
  int _currentTabIndex = 0;

  /// Returns the Fast preset's ModelSelected. Caller must have primed
  /// ModelsService().cached via a prior get() call.
  static ModelSelected getFastPreset() =>
      ModelsService().cached!.fastPreset!.models;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(length: 4, vsync: this);
    _tabController.addListener(_handleTabChange);
    _skillItems = initSkillItems();
    _modelsFuture = ModelsService().get();
  }

  void _initFromData(ModelsData data) {
    _functions = buildFunctions(data);
    _skillItems = initSkillItems(data);

    _selectedModel = widget.selectedModel.text?.name ??
        (data.textModelIds.isNotEmpty ? data.textModelIds.first : '');
    _selectedVisionModel = widget.selectedModel.vision?.name ??
        (data.visionModelIds.isNotEmpty ? data.visionModelIds.first : '');
    _selectedImageGenModel = widget.selectedModel.imageGen?.name ??
        (data.imageGenModelIds.isNotEmpty ? data.imageGenModelIds.first : '');

    final toolsMap = widget.selectedModel.text?.tools ?? {};
    if (toolsMap.isNotEmpty) {
      for (var function in _functions) {
        if (function['tools'] == null) {
          final mode = toolsMap[function['key']];
          function['enabled'] = _mapValueToToggle(mode);
        } else {
          bool allEnabled = true;
          String bundleMode = 'off';
          for (var tool in function['tools']) {
            final mode = toolsMap[tool];
            if (mode != null) {
              bundleMode = _mapValueToToggle(mode);
            } else {
              allEnabled = false;
              break;
            }
          }
          function['enabled'] = allEnabled ? bundleMode : 'off';
        }
      }

      for (var skill in _skillItems) {
        final mode = toolsMap[skill['key']];
        skill['enabled'] = _mapValueToToggle(mode);
      }
    }

    WidgetsBinding.instance.addPostFrameCallback((_) {
      _checkAndSetDefaultModels(data);
    });
  }

  String _getSelectedPresetName(List<PresetConfig> presets) {
    for (final preset in presets) {
      if (preset.models.text?.name == _selectedModel &&
          preset.models.vision?.name == _selectedVisionModel &&
          preset.models.imageGen?.name == _selectedImageGenModel) {
        return preset.name;
      }
    }
    return '';
  }

  void _handleTabChange() {
    setState(() {
      _currentTabIndex = _tabController.index;
    });
  }

  void _checkAndSetDefaultModels(ModelsData data) {
    final balanceState = ref.read(balanceProvider);
    final balance = balanceState.value;
    final isFree = balance?.planName == 'Free';

    final fast = data.fastPreset;
    if (isFree && fast != null) {
      final fastText = fast.models.text?.name;
      final fastVision = fast.models.vision?.name;
      final fastImage = fast.models.imageGen?.name;
      if (_selectedVisionModel != fastVision ||
          _selectedImageGenModel != fastImage ||
          _selectedModel != fastText) {
        setState(() {
          _selectedModel = fastText ?? _selectedModel;
          _selectedVisionModel = fastVision ?? _selectedVisionModel;
          _selectedImageGenModel = fastImage ?? _selectedImageGenModel;
        });
        widget.onModelSelected(fast.models);
      }
    }

    if (!data.textModelIds.contains(_selectedModel) && data.textModelIds.isNotEmpty) {
      setState(() => _selectedModel = data.textModelIds.first);
    }
    if (!data.visionModelIds.contains(_selectedVisionModel) && data.visionModelIds.isNotEmpty) {
      setState(() => _selectedVisionModel = data.visionModelIds.first);
    }
    if (!data.imageGenModelIds.contains(_selectedImageGenModel) && data.imageGenModelIds.isNotEmpty) {
      setState(() => _selectedImageGenModel = data.imageGenModelIds.first);
    }
  }

  @override
  void dispose() {
    _tabController.removeListener(_handleTabChange);
    _tabController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16.0)),
      elevation: 0,
      backgroundColor: Colors.transparent,
      child: FutureBuilder<ModelsData>(
        future: _modelsFuture,
        builder: (context, snapshot) {
          if (snapshot.connectionState != ConnectionState.done) {
            return _wrap(const SizedBox(
              height: 400,
              child: Center(child: CircularProgressIndicator()),
            ));
          }
          if (snapshot.hasError) {
            return _wrap(_errorBox(snapshot.error.toString()));
          }
          final data = snapshot.data!;
          if (_functions.isEmpty) {
            _initFromData(data);
          }
          return contentBox(context, data);
        },
      ),
    );
  }

  Widget _wrap(Widget child) {
    return Container(
      constraints: const BoxConstraints(maxWidth: 500),
      padding: const EdgeInsets.all(20),
      decoration: BoxDecoration(
        shape: BoxShape.rectangle,
        color: Theme.of(context).scaffoldBackgroundColor,
        borderRadius: BorderRadius.circular(16),
        boxShadow: const [
          BoxShadow(color: Colors.black26, blurRadius: 10.0, offset: Offset(0.0, 10.0)),
        ],
      ),
      child: child,
    );
  }

  Widget _errorBox(String message) {
    return SizedBox(
      height: 200,
      child: Column(
        mainAxisAlignment: MainAxisAlignment.center,
        children: [
          const Icon(Icons.error_outline, color: Colors.red, size: 40),
          const SizedBox(height: 12),
          Text('Failed to load models', style: Theme.of(context).textTheme.titleMedium),
          const SizedBox(height: 4),
          Text(message, style: Theme.of(context).textTheme.bodySmall, textAlign: TextAlign.center),
          const SizedBox(height: 16),
          ElevatedButton(
            onPressed: () {
              setState(() {
                _modelsFuture = ModelsService().get();
              });
            },
            child: const Text('Retry'),
          ),
        ],
      ),
    );
  }

  Widget contentBox(BuildContext context, ModelsData data) {
    final balanceState = ref.watch(balanceProvider);
    final balance = balanceState.value;
    final isFree = balance?.planName == 'Free';
    final isDarkMode = Theme.of(context).brightness == Brightness.dark;

    return _wrap(SingleChildScrollView(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          const Text(
            'Select AI Models',
            style: TextStyle(fontSize: 22, fontWeight: FontWeight.w600),
          ),
          const SizedBox(height: 16),

          TabBar(
            controller: _tabController,
            tabs: const [
              Tab(text: 'Presets'),
              Tab(text: 'Custom'),
              Tab(text: 'Functions'),
              Tab(text: 'Skills'),
            ],
            labelColor: Theme.of(context).colorScheme.primary,
            unselectedLabelColor: isDarkMode ? Colors.white : Colors.black,
            indicatorColor: Theme.of(context).colorScheme.primary,
          ),

          const SizedBox(height: 24),

          SizedBox(
            height: _currentTabIndex == 0 ? 300 : 300,
            child: TabBarView(
              controller: _tabController,
              children: [
                _buildPresetsTab(data, isFree),
                _buildCustomTab(data, isFree),
                _buildFunctionsTab(),
                _buildSkillsTab(),
              ],
            ),
          ),

          if (isFree)
            Container(
              margin: const EdgeInsets.only(top: 16),
              padding: const EdgeInsets.all(10),
              decoration: BoxDecoration(
                color: Colors.red.shade100,
                borderRadius: BorderRadius.circular(8),
                border: Border.all(color: Colors.red),
              ),
              child: const Text(
                "Only subscribers can change the models, free users are restricted to the currently selected models",
                style: TextStyle(color: Colors.red, fontWeight: FontWeight.bold),
                textAlign: TextAlign.center,
              ),
            ),

          const SizedBox(height: 24),

          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              if (!kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS))
                TextButton(
                  onPressed: () async {
                    final appDocumentDirectory = (await MCPService().getMCPPath()).path;
                    if (Platform.isWindows) {
                      Process.run('explorer', [appDocumentDirectory]);
                    } else if (Platform.isMacOS) {
                      Process.run('open', [appDocumentDirectory]);
                    } else if (Platform.isLinux) {
                      Process.run('xdg-open', [appDocumentDirectory]);
                    }
                  },
                  child: const Text('Edit MCP File (BETA)'),
                ),
              TextButton(
                onPressed: () => Navigator.pop(context),
                child: const Text('Cancel'),
              ),
              const SizedBox(width: 8),
              if (_currentTabIndex == 1 || _currentTabIndex == 2 || _currentTabIndex == 3)
                ElevatedButton(
                  onPressed: isFree
                      ? null
                      : () {
                          final Map<String, String> toolsMap = {};
                          for (final function in _functions) {
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
                          for (final skill in _skillItems) {
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

                          final selectedModels = _currentTabIndex == 0
                              ? _getSelectedPreset(data)
                              : ModelSelected(
                                  text: Model(
                                    name: _selectedModel,
                                    params: null,
                                    tools: toolsMap,
                                  ),
                                  vision: Model(
                                    name: _selectedVisionModel,
                                    params: null,
                                    tools: toolsMap,
                                  ),
                                  imageGen: Model(
                                    name: _selectedImageGenModel,
                                    params: null,
                                  ),
                                );

                          widget.onModelSelected(selectedModels);
                          Navigator.pop(context);
                        },
                  style: ElevatedButton.styleFrom(
                    backgroundColor: Theme.of(context).primaryColor,
                    foregroundColor: Colors.white,
                    disabledBackgroundColor: Colors.grey.shade300,
                    disabledForegroundColor: Colors.grey.shade500,
                  ),
                  child: const Text('Confirm'),
                ),
            ],
          ),
        ],
      ),
    ));
  }

  Widget _buildPresetsTab(ModelsData data, bool isFree) {
    final selectedName = _getSelectedPresetName(data.presets);
    return ListView.separated(
      itemCount: data.presets.length,
      separatorBuilder: (_, __) => const SizedBox(height: 12),
      itemBuilder: (context, index) {
        final preset = data.presets[index];
        final color = _colorFromName(preset.color);
        return _buildPresetButton(
          preset,
          color.shade100,
          color,
          isFree,
          selectedName == preset.name,
        );
      },
    );
  }

  Widget _buildPresetButton(
    PresetConfig preset,
    Color color,
    Color colorSelected,
    bool isFree,
    bool isSelected,
  ) {
    return InkWell(
      onTap: isFree
          ? null
          : () {
              setState(() {
                _selectedModel = preset.models.text?.name ?? _selectedModel;
                _selectedVisionModel = preset.models.vision?.name ?? _selectedVisionModel;
                _selectedImageGenModel = preset.models.imageGen?.name ?? _selectedImageGenModel;
              });
              widget.onModelSelected(preset.models);
              Navigator.pop(context);
            },
      child: Container(
        width: double.infinity,
        padding: const EdgeInsets.all(16),
        decoration: BoxDecoration(
          color: color,
          borderRadius: BorderRadius.circular(8),
          border: isSelected
              ? Border.all(color: colorSelected, width: 3)
              : Border.all(color: color.withOpacity(0.5)),
        ),
        child: Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    preset.name,
                    style: TextStyle(
                      fontSize: 18,
                      fontWeight: FontWeight.bold,
                      color: Colors.grey[900],
                    ),
                  ),
                  const SizedBox(height: 4),
                  Text(
                    preset.label,
                    style: TextStyle(fontSize: 14, color: Colors.grey.shade700),
                  ),
                ],
              ),
            ),
            Text(
              preset.pricing,
              style: TextStyle(
                color: Colors.grey[900],
                fontSize: 16,
                fontWeight: FontWeight.bold,
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildCustomTab(ModelsData data, bool isFree) {
    return Column(
      children: [
        _buildDropdown(
          label: 'Text Model',
          value: _selectedModel,
          items: data.textModelIds,
          onChanged: isFree
              ? (value) {}
              : (value) => setState(() => _selectedModel = value!),
        ),
        const SizedBox(height: 16),
        _buildDropdown(
          label: 'Vision Model',
          value: _selectedVisionModel,
          items: data.visionModelIds,
          onChanged: isFree
              ? (value) {}
              : (value) => setState(() => _selectedVisionModel = value!),
        ),
        const SizedBox(height: 16),
        _buildDropdown(
          label: 'Image Generation Model',
          value: _selectedImageGenModel,
          items: data.imageGenModelIds,
          onChanged: isFree
              ? (value) {}
              : (value) => setState(() => _selectedImageGenModel = value!),
        ),
      ],
    );
  }

  Widget _buildToolToggle(String currentMode, ValueChanged<String> onChanged) {
    return SegmentedButton<String>(
      segments: const [
        ButtonSegment(value: 'off', label: Text('Off')),
        ButtonSegment(value: 'ask', label: Text('Ask')),
        ButtonSegment(value: 'on', label: Text('On')),
      ],
      selected: {currentMode},
      onSelectionChanged: (selected) => onChanged(selected.first),
      showSelectedIcon: false,
      style: ButtonStyle(
        visualDensity: VisualDensity.compact,
        tapTargetSize: MaterialTapTargetSize.shrinkWrap,
        padding: WidgetStateProperty.all(const EdgeInsets.symmetric(horizontal: 8)),
      ),
    );
  }

  Widget _buildFunctionsTab() {
    return ListView.builder(
      itemCount: _functions.length,
      itemBuilder: (context, index) {
        final function = _functions[index];
        final label = function['label'] +
            ((function['tools'] != null && function['tools'].length > 1)
                ? ' (${function['tools'].length} tools) '
                : '');
        final isServer = function['source'] == 'server';
        return ListTile(
          title: Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              if (isServer) ...[
                const Icon(Icons.cloud_outlined, size: 16),
                const SizedBox(width: 6),
              ],
              Flexible(child: Text(label)),
            ],
          ),
          subtitle: Text(function['description']),
          trailing: _buildToolToggle(
            function['enabled'] as String,
            (mode) => setState(() => _functions[index]['enabled'] = mode),
          ),
        );
      },
    );
  }

  Widget _buildSkillsTab() {
    if (_skillItems.isEmpty) {
      return Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            const Text(
              'No skills found',
              style: TextStyle(fontSize: 16, fontWeight: FontWeight.w500),
            ),
            const SizedBox(height: 8),
            const Text(
              'Create a folder with a SKILL.md file\nin your skills directory to get started.',
              textAlign: TextAlign.center,
              style: TextStyle(color: Colors.grey),
            ),
            const SizedBox(height: 16),
            if (!kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS))
              ElevatedButton.icon(
                onPressed: () async {
                  final skillsDir = await SkillsService().getSkillsPath();
                  final dirPath = skillsDir.path;
                  if (Platform.isWindows) {
                    Process.run('explorer', [dirPath]);
                  } else if (Platform.isMacOS) {
                    Process.run('open', [dirPath]);
                  } else if (Platform.isLinux) {
                    Process.run('xdg-open', [dirPath]);
                  }
                },
                icon: const Icon(Icons.folder_open, size: 18),
                label: const Text('Open Skills Folder'),
              ),
          ],
        ),
      );
    }

    return Column(
      children: [
        if (!kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS))
          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              TextButton.icon(
                onPressed: () async {
                  final skillsDir = await SkillsService().getSkillsPath();
                  final dirPath = skillsDir.path;
                  if (Platform.isWindows) {
                    Process.run('explorer', [dirPath]);
                  } else if (Platform.isMacOS) {
                    Process.run('open', [dirPath]);
                  } else if (Platform.isLinux) {
                    Process.run('xdg-open', [dirPath]);
                  }
                },
                icon: const Icon(Icons.folder_open, size: 18),
                label: const Text('Open Folder'),
              ),
              IconButton(
                onPressed: () async {
                  await SkillsService().initSkills();
                  setState(() {
                    _skillItems = initSkillItems();
                  });
                },
                icon: const Icon(Icons.refresh, size: 18),
                tooltip: 'Refresh skills',
              ),
            ],
          ),
        Expanded(
          child: ListView.builder(
            itemCount: _skillItems.length,
            itemBuilder: (context, index) {
              final skill = _skillItems[index];
              final isServer = skill['source'] == 'server';
              return ListTile(
                title: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    if (isServer) ...[
                      const Icon(Icons.cloud_outlined, size: 16),
                      const SizedBox(width: 6),
                    ],
                    Flexible(child: Text(skill['label'] as String)),
                  ],
                ),
                subtitle: Text(skill['description']),
                trailing: _buildToolToggle(
                  skill['enabled'] as String,
                  (mode) => setState(() => _skillItems[index]['enabled'] = mode),
                ),
              );
            },
          ),
        ),
      ],
    );
  }

  ModelSelected _getSelectedPreset(ModelsData data) {
    for (final preset in data.presets) {
      if (preset.models.text?.name == _selectedModel &&
          preset.models.vision?.name == _selectedVisionModel &&
          preset.models.imageGen?.name == _selectedImageGenModel) {
        return preset.models;
      }
    }
    return data.presets.first.models;
  }

  Widget _buildDropdown({
    required String label,
    required String value,
    required List<String> items,
    required Function(String?)? onChanged,
  }) {
    final effectiveValue = items.contains(value) ? value : (items.isNotEmpty ? items.first : '');
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          label,
          style: const TextStyle(fontSize: 16, fontWeight: FontWeight.w500),
        ),
        const SizedBox(height: 8),
        Container(
          decoration: BoxDecoration(
            border: Border.all(color: Colors.grey.shade300),
            borderRadius: BorderRadius.circular(8),
          ),
          child: DropdownButtonHideUnderline(
            child: DropdownButton<String>(
              value: effectiveValue.isEmpty ? null : effectiveValue,
              isExpanded: true,
              icon: const Icon(Icons.arrow_drop_down),
              padding: const EdgeInsets.symmetric(horizontal: 12),
              borderRadius: BorderRadius.circular(8),
              items: items
                  .map((String item) => DropdownMenuItem<String>(
                        value: item,
                        child: Text(item),
                      ))
                  .toList(),
              onChanged: onChanged,
            ),
          ),
        ),
      ],
    );
  }
}
