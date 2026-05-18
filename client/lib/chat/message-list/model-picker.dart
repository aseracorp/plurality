import 'dart:io' show Platform, Process;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/models_service.dart';
import 'package:plurality/widgets/model_config/model_config_form.dart';
import 'package:plurality/widgets/model_config/model_config_helpers.dart';
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

  List<Map<String, dynamic>> _functions = [];
  List<Map<String, dynamic>> _skillItems = [];

  String _selectedModel = '';
  String _selectedVisionModel = '';
  String _selectedImageGenModel = '';

  late TabController _tabController;
  int _currentTabIndex = 0;

  /// Returns the Fast preset's ModelSelected (un-overridden). Caller must
  /// have primed ModelsService().cached via a prior get() call.
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

    final toolsMap =
        widget.selectedModel.text?.tools ?? const <String, String>{};
    if (toolsMap.isNotEmpty) {
      applyToolsMapToFunctions(_functions, toolsMap);
      applyToolsMapToSkills(_skillItems, toolsMap);
    }

    WidgetsBinding.instance.addPostFrameCallback((_) {
      _checkAndSetDefaultModels(data);
    });
  }

  String _getSelectedPresetName(List<PresetConfig> presets) {
    for (final preset in presets) {
      if (presetNameMatches(preset.models.text?.name, _selectedModel) &&
          presetNameMatches(preset.models.vision?.name, _selectedVisionModel) &&
          presetNameMatches(
              preset.models.imageGen?.name, _selectedImageGenModel)) {
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
    if (!data.textModelIds.contains(_selectedModel) &&
        data.textModelIds.isNotEmpty) {
      setState(() => _selectedModel = data.textModelIds.first);
    }
    if (!data.visionModelIds.contains(_selectedVisionModel) &&
        data.visionModelIds.isNotEmpty) {
      setState(() => _selectedVisionModel = data.visionModelIds.first);
    }
    if (!data.imageGenModelIds.contains(_selectedImageGenModel) &&
        data.imageGenModelIds.isNotEmpty) {
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
          BoxShadow(
              color: Colors.black26,
              blurRadius: 10.0,
              offset: Offset(0.0, 10.0)),
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
          Text('Failed to load models',
              style: Theme.of(context).textTheme.titleMedium),
          const SizedBox(height: 4),
          Text(message,
              style: Theme.of(context).textTheme.bodySmall,
              textAlign: TextAlign.center),
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
            height: 300,
            child: TabBarView(
              controller: _tabController,
              children: [
                _buildPresetsTab(data),
                _buildCustomTab(data),
                _buildFunctionsTab(),
                _buildSkillsTab(),
              ],
            ),
          ),
          const SizedBox(height: 24),
          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              if (!kIsWeb &&
                  (Platform.isWindows || Platform.isLinux || Platform.isMacOS))
                TextButton(
                  onPressed: () async {
                    final appDocumentDirectory =
                        (await MCPService().getMCPPath()).path;
                    if (Platform.isWindows) {
                      Process.run('explorer', [appDocumentDirectory]);
                    } else if (Platform.isMacOS) {
                      Process.run('open', [appDocumentDirectory]);
                    } else if (Platform.isLinux) {
                      Process.run('xdg-open', [appDocumentDirectory]);
                    }
                  },
                  child: const Text('Edit MCP File'),
                ),
              TextButton(
                onPressed: () => Navigator.pop(context),
                child: const Text('Cancel'),
              ),
              const SizedBox(width: 8),
              if (_currentTabIndex == 1 ||
                  _currentTabIndex == 2 ||
                  _currentTabIndex == 3)
                ElevatedButton(
                  onPressed: () {
                    final toolsMap = toolsMapFromFunctionsAndSkills(
                      _functions,
                      _skillItems,
                    );
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

  Widget _buildPresetsTab(ModelsData data) {
    final selectedName = _getSelectedPresetName(data.presets);
    return ListView.separated(
      itemCount: data.presets.length,
      separatorBuilder: (_, __) => const SizedBox(height: 12),
      itemBuilder: (context, index) {
        final preset = data.presets[index];
        return PresetButton(
          preset: preset,
          isSelected: selectedName == preset.name,
          onTap: () {
            final merged = mergePresetOnto(preset.models, widget.selectedModel);
            setState(() {
              _selectedModel = merged.text?.name.isNotEmpty == true
                  ? merged.text!.name
                  : _selectedModel;
              _selectedVisionModel = merged.vision?.name.isNotEmpty == true
                  ? merged.vision!.name
                  : _selectedVisionModel;
              _selectedImageGenModel = merged.imageGen?.name.isNotEmpty == true
                  ? merged.imageGen!.name
                  : _selectedImageGenModel;
            });
            widget.onModelSelected(merged);
            Navigator.pop(context);
          },
        );
      },
    );
  }

  Widget _buildCustomTab(ModelsData data) {
    return CustomModelsForm(
      data: data,
      text: _selectedModel,
      vision: _selectedVisionModel,
      imageGen: _selectedImageGenModel,
      onText: (v) => setState(() => _selectedModel = v ?? ''),
      onVision: (v) => setState(() => _selectedVisionModel = v ?? ''),
      onImageGen: (v) => setState(() => _selectedImageGenModel = v ?? ''),
    );
  }

  Widget _buildFunctionsTab() {
    return FunctionsListView(
      functions: _functions,
      onChanged: (index, mode) =>
          setState(() => _functions[index]['enabled'] = mode),
    );
  }

  Widget _buildSkillsTab() {
    return SkillsListView(
      skillItems: _skillItems,
      onChanged: (index, mode) =>
          setState(() => _skillItems[index]['enabled'] = mode),
      onRefresh: () => setState(() {
        _skillItems = initSkillItems();
      }),
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
}
