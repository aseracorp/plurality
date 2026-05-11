import 'package:flutter/material.dart';

import '../api/mini-apps.dart';
import '../utils/types.dart';

/// Dropdown for picking a MiniApp/preset. Used by the cron and webhook
/// edit dialogs — same data source, same shape.
///
/// Loads the preset list on first build (or accepts a pre-fetched future
/// via [presetsFuture]). While loading, shows a disabled placeholder so
/// the surrounding form layout doesn't jump.
class PresetDropdown extends StatefulWidget {
  final String value;
  final ValueChanged<String> onChanged;
  final Future<List<MiniApp>>? presetsFuture;
  final String label;

  const PresetDropdown({
    super.key,
    required this.value,
    required this.onChanged,
    this.presetsFuture,
    this.label = 'Preset',
  });

  @override
  State<PresetDropdown> createState() => _PresetDropdownState();
}

class _PresetDropdownState extends State<PresetDropdown> {
  late Future<List<MiniApp>> _future;

  @override
  void initState() {
    super.initState();
    _future = widget.presetsFuture ?? MiniAppsService().getAllMiniApps();
  }

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<List<MiniApp>>(
      future: _future,
      builder: (context, snap) {
        final presets = snap.data ?? const <MiniApp>[];
        // If the current value isn't (yet) in the list, fall back to the
        // first preset so the Dropdown has a valid initial selection.
        String selected = widget.value;
        if (presets.isNotEmpty && !presets.any((p) => p.id == selected)) {
          selected = presets.first.id;
        }
        return DropdownButtonFormField<String>(
          initialValue: presets.isEmpty ? null : selected,
          decoration: InputDecoration(labelText: widget.label),
          items: [
            for (final p in presets)
              DropdownMenuItem(value: p.id, child: Text(p.name)),
          ],
          onChanged: (v) {
            if (v != null) widget.onChanged(v);
          },
        );
      },
    );
  }
}
