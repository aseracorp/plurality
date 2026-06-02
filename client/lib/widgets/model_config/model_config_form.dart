import 'dart:io' show Platform, Process;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:plurality/api/models_service.dart';
import 'package:plurality/api/skills_service.dart';
import 'package:plurality/utils/types.dart';

import 'model_config_helpers.dart';

/// SegmentedButton with Off / Ask / On states for tool/skill toggles.
class ToolToggleSegmented extends StatelessWidget {
  final String currentMode;
  final ValueChanged<String> onChanged;

  const ToolToggleSegmented({
    Key? key,
    required this.currentMode,
    required this.onChanged,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
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
}

/// A row laying out a label, description, and a trailing [ToolToggleSegmented].
/// On wide containers the toggle sits next to the label; on narrow containers
/// the toggle wraps to its own line below the description so the label is not
/// squeezed.
class _ToolRow extends StatelessWidget {
  final String label;
  final String description;
  final bool isServer;
  final String currentMode;
  final ValueChanged<String> onChanged;

  // Below this width the toggle won't fit comfortably next to the label, so we
  // drop it onto its own line.
  static const double _stackBelow = 360;

  const _ToolRow({
    Key? key,
    required this.label,
    required this.description,
    required this.isServer,
    required this.currentMode,
    required this.onChanged,
  }) : super(key: key);

  Widget _title(BuildContext context) {
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        if (isServer) ...[
          const Icon(Icons.cloud_outlined, size: 16),
          const SizedBox(width: 6),
        ],
        Flexible(
          child: Text(
            label,
            style: Theme.of(context).textTheme.titleSmall,
          ),
        ),
      ],
    );
  }

  @override
  Widget build(BuildContext context) {
    final toggle = ToolToggleSegmented(
      currentMode: currentMode,
      onChanged: onChanged,
    );
    return LayoutBuilder(
      builder: (context, constraints) {
        final stack = constraints.maxWidth < _stackBelow;
        return Padding(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
          child: stack
              ? Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    _title(context),
                    const SizedBox(height: 4),
                    Text(description),
                    const SizedBox(height: 8),
                    Align(alignment: Alignment.centerRight, child: toggle),
                  ],
                )
              : Row(
                  crossAxisAlignment: CrossAxisAlignment.center,
                  children: [
                    Expanded(
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          _title(context),
                          const SizedBox(height: 4),
                          Text(description),
                        ],
                      ),
                    ),
                    const SizedBox(width: 12),
                    toggle,
                  ],
                ),
        );
      },
    );
  }
}

/// Bordered, label-on-top dropdown for choosing one of [items].
///
/// When [autoLabel] is non-null, an extra entry with the empty string as its
/// value is prepended to the menu (rendered with [autoLabel] as its text).
/// Selecting it reports `''` to [onChanged], and passing `''` for [value]
/// selects it. Callers that don't pass [autoLabel] keep the existing
/// "always pick a concrete item" semantics.
class LabeledDropdown extends StatelessWidget {
  final String label;
  final String value;
  final List<String> items;
  final ValueChanged<String?>? onChanged;
  final String? autoLabel;

  const LabeledDropdown({
    Key? key,
    required this.label,
    required this.value,
    required this.items,
    required this.onChanged,
    this.autoLabel,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    final allowAuto = autoLabel != null;
    final effectiveItems = allowAuto ? <String>['', ...items] : items;
    final String effectiveValue;
    if (effectiveItems.contains(value)) {
      effectiveValue = value;
    } else if (allowAuto) {
      effectiveValue = '';
    } else {
      effectiveValue = items.isNotEmpty ? items.first : '';
    }
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
              value: (!allowAuto && effectiveValue.isEmpty)
                  ? null
                  : effectiveValue,
              isExpanded: true,
              icon: const Icon(Icons.arrow_drop_down),
              padding: const EdgeInsets.symmetric(horizontal: 12),
              borderRadius: BorderRadius.circular(8),
              items: effectiveItems
                  .map((String item) => DropdownMenuItem<String>(
                        value: item,
                        child: Text(
                          item.isEmpty && allowAuto ? autoLabel! : item,
                          style: item.isEmpty && allowAuto
                              ? const TextStyle(fontStyle: FontStyle.italic)
                              : null,
                        ),
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

/// The colored preset card used in the picker's Presets tab and in the
/// Model Shortcuts editor listing.
class PresetButton extends StatelessWidget {
  final PresetConfig preset;
  final bool isSelected;
  final VoidCallback onTap;
  final Widget? trailing;

  const PresetButton({
    Key? key,
    required this.preset,
    required this.isSelected,
    required this.onTap,
    this.trailing,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    final base = colorFromName(preset.color);
    final color = base.shade100;
    final colorSelected = base;
    return InkWell(
      onTap: onTap,
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
            if (trailing != null) ...[
              const SizedBox(width: 8),
              trailing!,
            ],
          ],
        ),
      ),
    );
  }
}

/// Three labeled dropdowns (text / vision / image-gen) for choosing model ids.
///
/// When [allowAuto] is true, each dropdown gains a leading "Auto" entry that
/// reports `''` to its callback — used by the preset editor so a preset can
/// be saved without pinning a specific model, deferring model selection to
/// the preset's `complexity` field (see `server/src/jobs/preset.go`).
class CustomModelsForm extends StatelessWidget {
  final ModelsData data;
  final String text;
  final String vision;
  final String imageGen;
  final String imageEdit;
  final ValueChanged<String?> onText;
  final ValueChanged<String?> onVision;
  final ValueChanged<String?> onImageGen;
  final ValueChanged<String?> onImageEdit;
  final bool allowAuto;

  const CustomModelsForm({
    Key? key,
    required this.data,
    required this.text,
    required this.vision,
    required this.imageGen,
    required this.imageEdit,
    required this.onText,
    required this.onVision,
    required this.onImageGen,
    required this.onImageEdit,
    this.allowAuto = false,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    const autoLabel = 'Auto (use complexity)';
    // Scrollable so the model dropdowns don't overflow the fixed-height tab
    // container they're hosted in (model-picker.dart / ModelConfigForm).
    return SingleChildScrollView(
      child: Column(
      children: [
        LabeledDropdown(
          label: 'Text Model',
          value: text,
          items: data.textModelIds,
          onChanged: onText,
          autoLabel: allowAuto ? autoLabel : null,
        ),
        const SizedBox(height: 16),
        LabeledDropdown(
          label: 'Vision Model',
          value: vision,
          items: data.visionModelIds,
          onChanged: onVision,
          autoLabel: allowAuto ? autoLabel : null,
        ),
        const SizedBox(height: 16),
        LabeledDropdown(
          label: 'Image Generation Model',
          value: imageGen,
          items: data.imageGenModelIds,
          onChanged: onImageGen,
          autoLabel: allowAuto ? autoLabel : null,
        ),
        const SizedBox(height: 16),
        LabeledDropdown(
          label: 'Image Edit Model',
          value: imageEdit,
          items: data.imageEditModelIds,
          onChanged: onImageEdit,
          autoLabel: allowAuto ? autoLabel : null,
        ),
      ],
      ),
    );
  }
}

/// ListView rendering the functions/tools with Off/Ask/On toggles.
class FunctionsListView extends StatelessWidget {
  final List<Map<String, dynamic>> functions;
  final void Function(int index, String mode) onChanged;

  const FunctionsListView({
    Key? key,
    required this.functions,
    required this.onChanged,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return ListView.builder(
      itemCount: functions.length,
      itemBuilder: (context, index) {
        final function = functions[index];
        final label = function['label'] +
            ((function['tools'] != null && function['tools'].length > 1)
                ? ' (${function['tools'].length} tools) '
                : '');
        return _ToolRow(
          label: label,
          description: function['description'] as String,
          isServer: function['source'] == 'server',
          currentMode: function['enabled'] as String,
          onChanged: (mode) => onChanged(index, mode),
        );
      },
    );
  }
}

/// ListView rendering skills with Off/Ask/On toggles, plus empty-state and
/// "open skills folder" affordance on desktop.
class SkillsListView extends StatelessWidget {
  final List<Map<String, dynamic>> skillItems;
  final void Function(int index, String mode) onChanged;
  final VoidCallback onRefresh;

  const SkillsListView({
    Key? key,
    required this.skillItems,
    required this.onChanged,
    required this.onRefresh,
  }) : super(key: key);

  bool get _isDesktop =>
      !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);

  Future<void> _openSkillsFolder() async {
    final skillsDir = await SkillsService().getSkillsPath();
    final dirPath = skillsDir.path;
    if (Platform.isWindows) {
      Process.run('explorer', [dirPath]);
    } else if (Platform.isMacOS) {
      Process.run('open', [dirPath]);
    } else if (Platform.isLinux) {
      Process.run('xdg-open', [dirPath]);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (skillItems.isEmpty) {
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
            if (_isDesktop)
              ElevatedButton.icon(
                onPressed: _openSkillsFolder,
                icon: const Icon(Icons.folder_open, size: 18),
                label: const Text('Open Skills Folder'),
              ),
          ],
        ),
      );
    }

    return Column(
      children: [
        if (_isDesktop)
          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              TextButton.icon(
                onPressed: _openSkillsFolder,
                icon: const Icon(Icons.folder_open, size: 18),
                label: const Text('Open Folder'),
              ),
              IconButton(
                onPressed: () async {
                  await SkillsService().initSkills();
                  onRefresh();
                },
                icon: const Icon(Icons.refresh, size: 18),
                tooltip: 'Refresh skills',
              ),
            ],
          ),
        Expanded(
          child: ListView.builder(
            itemCount: skillItems.length,
            itemBuilder: (context, index) {
              final skill = skillItems[index];
              return _ToolRow(
                label: skill['label'] as String,
                description: skill['description'] as String,
                isServer: skill['source'] == 'server',
                currentMode: skill['enabled'] as String,
                onChanged: (mode) => onChanged(index, mode),
              );
            },
          ),
        ),
      ],
    );
  }
}

/// Composite form that orchestrates the Custom / Functions / Skills tabs
/// against a single `ModelSelected`. Used by both the model picker dialog
/// and the Model Shortcuts editor. Owns local state; expose [commit] via
/// a [GlobalKey] to assemble the final selection.
class ModelConfigForm extends StatefulWidget {
  final ModelsData data;
  final ModelSelected initial;

  /// When true, the per-slot model dropdowns expose an "Auto" entry whose
  /// value is the empty string. An empty-string model name on save is
  /// preserved (not auto-replaced by the first available model). The preset
  /// editor uses this so a preset can be saved without pinning a concrete
  /// model — server `ResolvePreset` then picks the model from the preset's
  /// `complexity` field. Existing call sites (shortcut editor) leave this
  /// `false` and keep the old "always pick a concrete model" behaviour.
  final bool allowAutoModel;

  const ModelConfigForm({
    Key? key,
    required this.data,
    required this.initial,
    this.allowAutoModel = false,
  }) : super(key: key);

  @override
  ModelConfigFormState createState() => ModelConfigFormState();
}

class ModelConfigFormState extends State<ModelConfigForm>
    with SingleTickerProviderStateMixin {
  late TabController _tabController;
  late String _selectedModel;
  late String _selectedVisionModel;
  late String _selectedImageGenModel;
  late String _selectedImageEditModel;
  late List<Map<String, dynamic>> _functions;
  late List<Map<String, dynamic>> _skillItems;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(length: 3, vsync: this);
    _initFromData(widget.data);
  }

  void _initFromData(ModelsData data) {
    _functions = buildFunctions(data);
    _skillItems = initSkillItems(data);

    // For each slot, an explicit empty name is preserved when allowAutoModel
    // is true (it means "let complexity decide" on the server). Otherwise we
    // fall back to the first available model.
    String pickModel(String? name, List<String> available) {
      if (name == null) {
        return available.isNotEmpty ? available.first : '';
      }
      if (name.isEmpty && widget.allowAutoModel) return '';
      if (name.isEmpty) {
        return available.isNotEmpty ? available.first : '';
      }
      return name;
    }
    _selectedModel = pickModel(widget.initial.text?.name, data.textModelIds);
    _selectedVisionModel =
        pickModel(widget.initial.vision?.name, data.visionModelIds);
    _selectedImageGenModel =
        pickModel(widget.initial.imageGen?.name, data.imageGenModelIds);
    _selectedImageEditModel =
        pickModel(widget.initial.imageEdit?.name, data.imageEditModelIds);

    // When the caller specifies an `initial.text` (even with an empty tools
    // map), treat that as the authoritative selection. An explicit empty map
    // means "every tool off", which the preset editor relies on when opening
    // a preset whose `model_selected` doesn't list any tools. Only when
    // `text` is null do we keep the server `FunctionDef.defaultState`
    // values seeded by `buildFunctions(data)`.
    if (widget.initial.text != null) {
      final toolsMap = widget.initial.text!.tools;
      applyToolsMapToFunctions(_functions, toolsMap);
      applyToolsMapToSkills(_skillItems, toolsMap);
    }

    WidgetsBinding.instance.addPostFrameCallback((_) {
      _checkAndSetDefaultModels(data);
    });
  }

  void _checkAndSetDefaultModels(ModelsData data) {
    // Empty selection is a valid persistable state when allowAutoModel is
    // on (means "use complexity-based shortcut"); don't clobber it.
    bool keepEmpty(String v) => widget.allowAutoModel && v.isEmpty;
    if (!keepEmpty(_selectedModel) &&
        !data.textModelIds.contains(_selectedModel) &&
        data.textModelIds.isNotEmpty) {
      setState(() => _selectedModel = data.textModelIds.first);
    }
    if (!keepEmpty(_selectedVisionModel) &&
        !data.visionModelIds.contains(_selectedVisionModel) &&
        data.visionModelIds.isNotEmpty) {
      setState(() => _selectedVisionModel = data.visionModelIds.first);
    }
    if (!keepEmpty(_selectedImageGenModel) &&
        !data.imageGenModelIds.contains(_selectedImageGenModel) &&
        data.imageGenModelIds.isNotEmpty) {
      setState(() => _selectedImageGenModel = data.imageGenModelIds.first);
    }
    if (!keepEmpty(_selectedImageEditModel) &&
        !data.imageEditModelIds.contains(_selectedImageEditModel) &&
        data.imageEditModelIds.isNotEmpty) {
      setState(() => _selectedImageEditModel = data.imageEditModelIds.first);
    }
  }

  @override
  void dispose() {
    _tabController.dispose();
    super.dispose();
  }

  /// Assemble the current selection into a [ModelSelected].
  ModelSelected commit() {
    final toolsMap = toolsMapFromFunctionsAndSkills(_functions, _skillItems);
    return ModelSelected(
      text: Model(name: _selectedModel, params: null, tools: toolsMap),
      vision: Model(name: _selectedVisionModel, params: null, tools: toolsMap),
      imageGen: Model(name: _selectedImageGenModel, params: null),
      imageEdit: Model(name: _selectedImageEditModel, params: null),
    );
  }

  void _refreshSkills() {
    setState(() {
      _skillItems = initSkillItems(widget.data);
    });
  }

  @override
  Widget build(BuildContext context) {
    final isDarkMode = Theme.of(context).brightness == Brightness.dark;
    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        TabBar(
          controller: _tabController,
          tabs: const [
            Tab(text: 'Custom'),
            Tab(text: 'Functions'),
            Tab(text: 'Skills'),
          ],
          labelColor: Theme.of(context).colorScheme.primary,
          unselectedLabelColor: isDarkMode ? Colors.white : Colors.black,
          indicatorColor: Theme.of(context).colorScheme.primary,
        ),
        const SizedBox(height: 16),
        SizedBox(
          height: 300,
          child: TabBarView(
            controller: _tabController,
            children: [
              CustomModelsForm(
                data: widget.data,
                text: _selectedModel,
                vision: _selectedVisionModel,
                imageGen: _selectedImageGenModel,
                imageEdit: _selectedImageEditModel,
                allowAuto: widget.allowAutoModel,
                onText: (v) => setState(() => _selectedModel = v ?? ''),
                onVision: (v) =>
                    setState(() => _selectedVisionModel = v ?? ''),
                onImageGen: (v) =>
                    setState(() => _selectedImageGenModel = v ?? ''),
                onImageEdit: (v) =>
                    setState(() => _selectedImageEditModel = v ?? ''),
              ),
              FunctionsListView(
                functions: _functions,
                onChanged: (index, mode) =>
                    setState(() => _functions[index]['enabled'] = mode),
              ),
              SkillsListView(
                skillItems: _skillItems,
                onChanged: (index, mode) =>
                    setState(() => _skillItems[index]['enabled'] = mode),
                onRefresh: _refreshSkills,
              ),
            ],
          ),
        ),
      ],
    );
  }
}
