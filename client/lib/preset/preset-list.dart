import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/mini-apps.dart';
import '../api/models_service.dart';
import '../utils/types.dart';
import 'preset-editor.dart';

class PresetListScreen extends ConsumerStatefulWidget {
  const PresetListScreen({super.key});

  @override
  ConsumerState<PresetListScreen> createState() => _PresetListScreenState();
}

class _PresetListScreenState extends ConsumerState<PresetListScreen> {
  final MiniAppsService _service = MiniAppsService();
  final ModelsService _modelsService = ModelsService();

  List<MiniApp> _presets = [];
  Set<String> _pinnedIds = {};
  bool _loading = true;
  String? _error;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final results = await Future.wait([
        _service.getAllMiniApps(),
        _service.getUserPinnedMiniApps(),
      ]);
      if (!mounted) return;
      setState(() {
        _presets = results[0];
        _pinnedIds = results[1].map((p) => p.id).toSet();
        _loading = false;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _loading = false;
      });
    }
  }

  Future<void> _openEditor({MiniApp? existing}) async {
    final ModelsData modelsData;
    try {
      modelsData = await _modelsService.get();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to load models: $e')),
      );
      return;
    }
    if (!mounted) return;

    final saved = await showDialog<MiniApp>(
      context: context,
      builder: (ctx) => PresetEditorDialog(
        modelsData: modelsData,
        existing: existing,
      ),
    );
    if (saved == null) return;

    try {
      if (existing == null) {
        await _service.createMiniApp(saved);
      } else {
        await _service.updateMiniApp(existing.id, saved);
      }
      await _load();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to save preset: $e')),
      );
    }
  }

  Future<void> _togglePin(MiniApp preset) async {
    final isPinned = _pinnedIds.contains(preset.id);
    try {
      if (isPinned) {
        await _service.unpinMiniApp(preset.id);
      } else {
        await _service.pinMiniApp(preset.id);
      }
      if (!mounted) return;
      setState(() {
        if (isPinned) {
          _pinnedIds.remove(preset.id);
        } else {
          _pinnedIds.add(preset.id);
        }
      });
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to update pin: $e')),
      );
    }
  }

  Future<void> _confirmDelete(MiniApp preset) async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Delete preset?'),
        content: Text(
          'Are you sure you want to delete "${preset.name}"? This cannot be undone.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: Colors.red),
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('Delete'),
          ),
        ],
      ),
    );
    if (ok != true) return;
    try {
      await _service.deleteMiniApp(preset.id);
      await _load();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to delete preset: $e')),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Presets'),
        actions: [
          IconButton(
            tooltip: 'New preset',
            icon: const Icon(Icons.add),
            onPressed: () => _openEditor(),
          ),
        ],
      ),
      body: RefreshIndicator(
        onRefresh: _load,
        child: _buildBody(),
      ),
      floatingActionButton: FloatingActionButton(
        onPressed: () => _openEditor(),
        tooltip: 'New preset',
        child: const Icon(Icons.add),
      ),
    );
  }

  Widget _buildBody() {
    if (_loading) {
      return const Center(child: CircularProgressIndicator());
    }
    if (_error != null) {
      return ListView(
        children: [
          const SizedBox(height: 80),
          Center(
            child: Column(
              children: [
                const Icon(Icons.error_outline, color: Colors.red, size: 36),
                const SizedBox(height: 8),
                Padding(
                  padding: const EdgeInsets.symmetric(horizontal: 24),
                  child: Text(_error!, textAlign: TextAlign.center),
                ),
                const SizedBox(height: 12),
                ElevatedButton(
                  onPressed: _load,
                  child: const Text('Retry'),
                ),
              ],
            ),
          ),
        ],
      );
    }
    if (_presets.isEmpty) {
      return ListView(
        children: const [
          SizedBox(height: 96),
          Center(
            child: Padding(
              padding: EdgeInsets.all(24),
              child: Text(
                'No presets yet.\nTap + to create one.',
                textAlign: TextAlign.center,
                style: TextStyle(fontSize: 16),
              ),
            ),
          ),
        ],
      );
    }
    return ListView.separated(
      itemCount: _presets.length,
      separatorBuilder: (_, __) => const Divider(height: 1),
      itemBuilder: (context, i) {
        final p = _presets[i];
        final pinned = _pinnedIds.contains(p.id);
        return _PresetTile(
          preset: p,
          pinned: pinned,
          onTogglePin: () => _togglePin(p),
          onEdit: () => _openEditor(existing: p),
          onDelete: () => _confirmDelete(p),
        );
      },
    );
  }
}

class _PresetTile extends StatelessWidget {
  final MiniApp preset;
  final bool pinned;
  final VoidCallback onTogglePin;
  final VoidCallback onEdit;
  final VoidCallback onDelete;

  const _PresetTile({
    required this.preset,
    required this.pinned,
    required this.onTogglePin,
    required this.onEdit,
    required this.onDelete,
  });

  @override
  Widget build(BuildContext context) {
    return ListTile(
      onTap: onEdit,
      leading: Container(
        width: 48,
        height: 48,
        decoration: BoxDecoration(
          color: Theme.of(context).colorScheme.surfaceVariant,
          borderRadius: BorderRadius.circular(8),
        ),
        clipBehavior: Clip.antiAlias,
        child: preset.iconURL.isNotEmpty
            ? Image.memory(base64Decode(preset.iconURL), fit: BoxFit.cover)
            : Icon(
                Icons.extension,
                color: Theme.of(context).colorScheme.primary,
              ),
      ),
      title: Text(
        preset.name,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
      ),
      subtitle: preset.description.isEmpty
          ? null
          : Text(
              preset.description,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
            ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          IconButton(
            tooltip: pinned ? 'Unpin' : 'Pin to home',
            icon: Icon(
              pinned ? Icons.star : Icons.star_border,
              color: pinned ? Colors.amber : null,
            ),
            onPressed: onTogglePin,
          ),
          IconButton(
            tooltip: 'Edit',
            icon: const Icon(Icons.edit_outlined),
            onPressed: onEdit,
          ),
          IconButton(
            tooltip: 'Delete',
            icon: const Icon(Icons.delete_outline),
            onPressed: onDelete,
          ),
        ],
      ),
    );
  }
}
