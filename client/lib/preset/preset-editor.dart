import 'dart:convert';
import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';

import '../api/models_service.dart';
import '../utils/types.dart';
import '../widgets/model_config/model_config_form.dart';

class PresetEditorDialog extends StatefulWidget {
  final ModelsData modelsData;
  final MiniApp? existing;

  const PresetEditorDialog({
    super.key,
    required this.modelsData,
    this.existing,
  });

  @override
  State<PresetEditorDialog> createState() => _PresetEditorDialogState();
}

class _PresetEditorDialogState extends State<PresetEditorDialog> {
  late final TextEditingController _nameCtrl;
  late final TextEditingController _descCtrl;
  late final TextEditingController _placeholderCtrl;
  late final TextEditingController _initialMessageCtrl;

  late String _iconBase64;
  late String _complexity;

  final _formKey = GlobalKey<ModelConfigFormState>();

  @override
  void initState() {
    super.initState();
    final e = widget.existing;
    _nameCtrl = TextEditingController(text: e?.name ?? '');
    _descCtrl = TextEditingController(text: e?.description ?? '');
    _placeholderCtrl = TextEditingController(text: e?.placeholder ?? '');
    _initialMessageCtrl =
        TextEditingController(text: e?.initialMessage?['en'] ?? '');
    _iconBase64 = e?.iconURL ?? '';
    final c = (e?.complexity ?? '').toLowerCase();
    _complexity = (c == 'low' || c == 'medium' || c == 'high') ? c : 'medium';
  }

  @override
  void dispose() {
    _nameCtrl.dispose();
    _descCtrl.dispose();
    _placeholderCtrl.dispose();
    _initialMessageCtrl.dispose();
    super.dispose();
  }

  /// Build the initial `ModelSelected` for the embedded picker. A null slot
  /// on the preset means "no opinion" in the JSON — we materialise it as an
  /// explicit empty `Model` so the form renders the slot as a blank slate
  /// (first available model + every tool off) rather than inheriting the
  /// server's `FunctionDef.defaultState` values or the user's currently
  /// selected tools. The form's `_initFromData` treats a non-null `text`
  /// with an empty `tools` map as authoritative all-off.
  ModelSelected _initialModelSelected() {
    final preset = widget.existing?.modelSelected;
    const blank = Model(name: '', params: null, tools: {});
    return ModelSelected(
      text: preset?.text ?? blank,
      vision: preset?.vision ?? blank,
      imageGen: preset?.imageGen ?? blank,
      audioGen: preset?.audioGen,
      voiceGen: preset?.voiceGen,
      audioTranscribe: preset?.audioTranscribe,
      videoGen: preset?.videoGen,
      videoVision: preset?.videoVision,
      code: preset?.code,
    );
  }

  Future<void> _pickIcon() async {
    final result = await FilePicker.platform.pickFiles(
      type: FileType.image,
      withData: true,
    );
    final bytes = result?.files.single.bytes;
    if (bytes == null) return;
    setState(() => _iconBase64 = base64Encode(bytes));
  }

  void _save() {
    final name = _nameCtrl.text.trim();
    if (name.isEmpty) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Name is required')),
      );
      return;
    }

    final modelSelected = _formKey.currentState?.commit();
    final initialMessageText = _initialMessageCtrl.text.trim();
    final e = widget.existing;

    final saved = MiniApp(
      id: e?.id ?? '',
      name: name,
      description: _descCtrl.text.trim(),
      iconURL: _iconBase64,
      author: e?.author,
      modelSelected: modelSelected,
      inputs: e?.inputs ?? const [],
      initialMessage:
          initialMessageText.isEmpty ? null : {'en': initialMessageText},
      form: e?.form ?? '',
      placeholder: _placeholderCtrl.text.trim(),
      complexity: _complexity,
    );

    Navigator.of(context).pop<MiniApp>(saved);
  }

  @override
  Widget build(BuildContext context) {
    final isEditing = widget.existing != null;
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 640, maxHeight: 720),
        child: Padding(
          padding: const EdgeInsets.all(20),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Text(
                isEditing ? 'Edit Preset' : 'New Preset',
                style: const TextStyle(
                  fontSize: 20,
                  fontWeight: FontWeight.w600,
                ),
              ),
              const SizedBox(height: 16),
              Expanded(
                child: SingleChildScrollView(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: [
                      _iconRow(context),
                      const SizedBox(height: 12),
                      TextField(
                        controller: _nameCtrl,
                        decoration: const InputDecoration(
                          labelText: 'Name',
                        ),
                      ),
                      const SizedBox(height: 12),
                      TextField(
                        controller: _descCtrl,
                        decoration: const InputDecoration(
                          labelText: 'Description',
                        ),
                        maxLines: 3,
                        minLines: 2,
                      ),
                      const SizedBox(height: 12),
                      TextField(
                        controller: _placeholderCtrl,
                        decoration: const InputDecoration(
                          labelText: 'Input placeholder',
                          hintText: 'e.g. "Describe your idea..."',
                        ),
                      ),
                      const SizedBox(height: 12),
                      TextField(
                        controller: _initialMessageCtrl,
                        decoration: const InputDecoration(
                          labelText: 'Initial message',
                          hintText:
                              'Shown as a quote on the preset start screen',
                        ),
                        maxLines: 4,
                        minLines: 2,
                      ),
                      const SizedBox(height: 16),
                      _complexityRow(context),
                      const SizedBox(height: 16),
                      const Divider(),
                      const SizedBox(height: 8),
                      Align(
                        alignment: Alignment.centerLeft,
                        child: Text(
                          'Models & tools',
                          style: Theme.of(context).textTheme.titleMedium,
                        ),
                      ),
                      const SizedBox(height: 8),
                      ModelConfigForm(
                        key: _formKey,
                        data: widget.modelsData,
                        initial: _initialModelSelected(),
                        allowAutoModel: true,
                      ),
                    ],
                  ),
                ),
              ),
              const SizedBox(height: 16),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  TextButton(
                    onPressed: () => Navigator.of(context).pop(),
                    child: const Text('Cancel'),
                  ),
                  const SizedBox(width: 8),
                  ElevatedButton(
                    onPressed: _save,
                    style: ElevatedButton.styleFrom(
                      backgroundColor: Theme.of(context).primaryColor,
                      foregroundColor: Colors.white,
                    ),
                    child: const Text('Save'),
                  ),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }

  Widget _iconRow(BuildContext context) {
    return Row(
      children: [
        Container(
          width: 64,
          height: 64,
          decoration: BoxDecoration(
            color: Theme.of(context).colorScheme.surfaceVariant,
            borderRadius: BorderRadius.circular(12),
          ),
          clipBehavior: Clip.antiAlias,
          child: _iconBase64.isNotEmpty
              ? Image.memory(base64Decode(_iconBase64), fit: BoxFit.cover)
              : Icon(
                  Icons.extension,
                  size: 32,
                  color: Theme.of(context).colorScheme.primary,
                ),
        ),
        const SizedBox(width: 12),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                'Icon',
                style: Theme.of(context).textTheme.titleSmall,
              ),
              const SizedBox(height: 4),
              Row(
                children: [
                  OutlinedButton.icon(
                    onPressed: _pickIcon,
                    icon: const Icon(Icons.image_outlined, size: 18),
                    label: Text(_iconBase64.isEmpty ? 'Pick image' : 'Change'),
                  ),
                  if (_iconBase64.isNotEmpty) ...[
                    const SizedBox(width: 8),
                    TextButton(
                      onPressed: () => setState(() => _iconBase64 = ''),
                      child: const Text('Remove'),
                    ),
                  ],
                ],
              ),
            ],
          ),
        ),
      ],
    );
  }

  Widget _complexityRow(BuildContext context) {
    return Row(
      crossAxisAlignment: CrossAxisAlignment.center,
      children: [
        Text(
          'Complexity',
          style: Theme.of(context).textTheme.titleSmall,
        ),
        const SizedBox(width: 16),
        Expanded(
          child: SegmentedButton<String>(
            segments: const [
              ButtonSegment(value: 'low', label: Text('Low')),
              ButtonSegment(value: 'medium', label: Text('Medium')),
              ButtonSegment(value: 'high', label: Text('High')),
            ],
            selected: {_complexity},
            onSelectionChanged: (v) =>
                setState(() => _complexity = v.first),
          ),
        ),
      ],
    );
  }
}
