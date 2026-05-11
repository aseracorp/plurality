import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/mini-apps.dart';
import '../api/webhook-service.dart';
import '../utils/types.dart';
import '../widgets/preset_dropdown.dart';

const _defaultPresetId = 'default-background-agent';

class WebhookListScreen extends ConsumerStatefulWidget {
  const WebhookListScreen({super.key});

  @override
  ConsumerState<WebhookListScreen> createState() => _WebhookListScreenState();
}

class _WebhookListScreenState extends ConsumerState<WebhookListScreen> {
  Future<List<MiniApp>>? _presetsFuture;

  @override
  void initState() {
    super.initState();
    _presetsFuture = MiniAppsService().getAllMiniApps();
  }

  @override
  Widget build(BuildContext context) {
    final hooks = ref.watch(webhooksProvider);

    return Scaffold(
      appBar: AppBar(title: const Text('Webhooks')),
      body: RefreshIndicator(
        onRefresh: () => ref.read(webhooksProvider.notifier).refresh(),
        child: hooks.isEmpty
            ? ListView(
                children: const [
                  SizedBox(height: 96),
                  Center(
                    child: Padding(
                      padding: EdgeInsets.all(24),
                      child: Text(
                        'No webhooks yet.\nTap + to add one.',
                        textAlign: TextAlign.center,
                        style: TextStyle(fontSize: 16),
                      ),
                    ),
                  ),
                ],
              )
            : ListView.separated(
                itemCount: hooks.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (context, i) {
                  final w = hooks[i];
                  return _WebhookTile(
                    hook: w,
                    presetsFuture: _presetsFuture,
                    onEdit: () => _openEditDialog(w),
                  );
                },
              ),
      ),
      floatingActionButton: FloatingActionButton(
        onPressed: _openCreateDialog,
        child: const Icon(Icons.add),
      ),
    );
  }

  Future<void> _openCreateDialog() async {
    final promptCtrl = TextEditingController();
    String selectedPreset = _defaultPresetId;

    final result = await showDialog<WebhookCreateResult>(
      context: context,
      builder: (ctx) {
        return StatefulBuilder(
          builder: (ctx, setLocal) {
            return AlertDialog(
              title: const Text('New Webhook'),
              content: SingleChildScrollView(
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    TextField(
                      controller: promptCtrl,
                      decoration: const InputDecoration(labelText: 'Prompt'),
                      maxLines: 5,
                      minLines: 2,
                    ),
                    const SizedBox(height: 12),
                    PresetDropdown(
                      value: selectedPreset,
                      presetsFuture: _presetsFuture,
                      onChanged: (v) => setLocal(() => selectedPreset = v),
                    ),
                  ],
                ),
              ),
              actions: [
                TextButton(
                  onPressed: () => Navigator.of(ctx).pop(),
                  child: const Text('Cancel'),
                ),
                FilledButton(
                  onPressed: () async {
                    final prompt = promptCtrl.text.trim();
                    if (prompt.isEmpty) {
                      ScaffoldMessenger.of(ctx).showSnackBar(
                        const SnackBar(content: Text('Prompt is required')),
                      );
                      return;
                    }
                    try {
                      final res = await ref.read(webhooksProvider.notifier).add(
                            prompt: prompt,
                            presetId: selectedPreset,
                          );
                      if (ctx.mounted) Navigator.of(ctx).pop(res);
                    } catch (e) {
                      if (ctx.mounted) {
                        ScaffoldMessenger.of(ctx).showSnackBar(
                          SnackBar(content: Text('Error: $e')),
                        );
                      }
                    }
                  },
                  child: const Text('Create'),
                ),
              ],
            );
          },
        );
      },
    );

    promptCtrl.dispose();

    if (result != null && mounted) {
      await _showTokenDialog(
        title: 'Webhook created',
        url: result.url,
        token: result.token,
      );
    }
  }

  Future<void> _openEditDialog(Webhook existing) async {
    final promptCtrl = TextEditingController(text: existing.prompt);
    String selectedPreset =
        existing.presetId.isNotEmpty ? existing.presetId : _defaultPresetId;

    await showDialog<void>(
      context: context,
      builder: (ctx) {
        return StatefulBuilder(
          builder: (ctx, setLocal) {
            return AlertDialog(
              title: const Text('Edit Webhook'),
              content: SingleChildScrollView(
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    TextField(
                      controller: promptCtrl,
                      decoration: const InputDecoration(labelText: 'Prompt'),
                      maxLines: 5,
                      minLines: 2,
                    ),
                    const SizedBox(height: 12),
                    PresetDropdown(
                      value: selectedPreset,
                      presetsFuture: _presetsFuture,
                      onChanged: (v) => setLocal(() => selectedPreset = v),
                    ),
                  ],
                ),
              ),
              actions: [
                TextButton(
                  onPressed: () => Navigator.of(ctx).pop(),
                  child: const Text('Cancel'),
                ),
                FilledButton(
                  onPressed: () async {
                    final prompt = promptCtrl.text.trim();
                    if (prompt.isEmpty) {
                      ScaffoldMessenger.of(ctx).showSnackBar(
                        const SnackBar(content: Text('Prompt is required')),
                      );
                      return;
                    }
                    try {
                      await ref.read(webhooksProvider.notifier).update(
                            existing.id,
                            prompt: prompt,
                            presetId: selectedPreset,
                          );
                      if (ctx.mounted) Navigator.of(ctx).pop();
                    } catch (e) {
                      if (ctx.mounted) {
                        ScaffoldMessenger.of(ctx).showSnackBar(
                          SnackBar(content: Text('Error: $e')),
                        );
                      }
                    }
                  },
                  child: const Text('Save'),
                ),
              ],
            );
          },
        );
      },
    );

    promptCtrl.dispose();
  }

  Future<void> _showTokenDialog({
    required String title,
    required String url,
    required String token,
  }) async {
    await showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const Text(
              'Copy this URL now — the token cannot be recovered afterwards.',
              style: TextStyle(fontWeight: FontWeight.w500),
            ),
            const SizedBox(height: 12),
            SelectableText(
              url,
              style: const TextStyle(fontFamily: 'monospace', fontSize: 13),
            ),
            const SizedBox(height: 12),
            Row(
              children: [
                Expanded(
                  child: OutlinedButton.icon(
                    icon: const Icon(Icons.copy, size: 18),
                    label: const Text('Copy URL'),
                    onPressed: () async {
                      await Clipboard.setData(ClipboardData(text: url));
                      if (ctx.mounted) {
                        ScaffoldMessenger.of(ctx).showSnackBar(
                          const SnackBar(content: Text('Copied URL')),
                        );
                      }
                    },
                  ),
                ),
                const SizedBox(width: 8),
                Expanded(
                  child: OutlinedButton.icon(
                    icon: const Icon(Icons.key, size: 18),
                    label: const Text('Copy token'),
                    onPressed: () async {
                      await Clipboard.setData(ClipboardData(text: token));
                      if (ctx.mounted) {
                        ScaffoldMessenger.of(ctx).showSnackBar(
                          const SnackBar(content: Text('Copied token')),
                        );
                      }
                    },
                  ),
                ),
              ],
            ),
            const SizedBox(height: 8),
            const Text(
              'You can pass the token via ?token=... in the URL OR via the\nX-WEBHOOK-TOKEN header.',
              style: TextStyle(fontSize: 12, color: Colors.grey),
            ),
          ],
        ),
        actions: [
          FilledButton(
            onPressed: () => Navigator.of(ctx).pop(),
            child: const Text('Done'),
          ),
        ],
      ),
    );
  }
}

class _WebhookTile extends ConsumerWidget {
  final Webhook hook;
  final Future<List<MiniApp>>? presetsFuture;
  final VoidCallback onEdit;

  const _WebhookTile({
    required this.hook,
    required this.presetsFuture,
    required this.onEdit,
  });

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final notifier = ref.read(webhooksProvider.notifier);
    final lastTriggered = hook.lastTriggeredAt;

    return ListTile(
      isThreeLine: true,
      title: Text(
        hook.prompt,
        maxLines: 2,
        overflow: TextOverflow.ellipsis,
        style: TextStyle(
          color: hook.enabled ? null : Theme.of(context).disabledColor,
        ),
      ),
      subtitle: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          FutureBuilder<List<MiniApp>>(
            future: presetsFuture,
            builder: (context, snap) {
              final name = snap.data
                      ?.where((p) => p.id == hook.presetId)
                      .map((p) => p.name)
                      .firstOrNull ??
                  hook.presetId;
              return Text(
                name,
                style: const TextStyle(fontSize: 12, color: Colors.grey),
              );
            },
          ),
          Text(
            lastTriggered != null
                ? 'Last triggered: ${lastTriggered.toLocal()}'
                : 'Never triggered',
            style: const TextStyle(fontSize: 12, color: Colors.grey),
          ),
        ],
      ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Switch(
            value: hook.enabled,
            onChanged: (v) => notifier.update(hook.id, enabled: v),
          ),
          IconButton(
            icon: const Icon(Icons.refresh),
            tooltip: 'Rotate token',
            onPressed: () => _rotate(context, notifier),
          ),
          IconButton(
            icon: const Icon(Icons.edit),
            tooltip: 'Edit',
            onPressed: onEdit,
          ),
          IconButton(
            icon: const Icon(Icons.delete),
            tooltip: 'Delete',
            onPressed: () => _confirmDelete(context, notifier),
          ),
        ],
      ),
    );
  }

  Future<void> _rotate(BuildContext context, WebhooksNotifier notifier) async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Rotate token?'),
        content: const Text(
          'The current token will stop working immediately. '
          'You will be shown the new URL exactly once.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('Rotate'),
          ),
        ],
      ),
    );
    if (ok != true || !context.mounted) return;

    try {
      final result = await notifier.rotateToken(hook.id);
      if (!context.mounted) return;
      await showDialog<void>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: const Text('New token'),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              const Text(
                'Copy this URL now — the token cannot be recovered afterwards.',
                style: TextStyle(fontWeight: FontWeight.w500),
              ),
              const SizedBox(height: 12),
              SelectableText(
                result.url,
                style: const TextStyle(fontFamily: 'monospace', fontSize: 13),
              ),
              const SizedBox(height: 12),
              OutlinedButton.icon(
                icon: const Icon(Icons.copy, size: 18),
                label: const Text('Copy URL'),
                onPressed: () async {
                  await Clipboard.setData(ClipboardData(text: result.url));
                  if (ctx.mounted) {
                    ScaffoldMessenger.of(ctx).showSnackBar(
                      const SnackBar(content: Text('Copied URL')),
                    );
                  }
                },
              ),
            ],
          ),
          actions: [
            FilledButton(
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('Done'),
            ),
          ],
        ),
      );
    } catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text('Error: $e')),
        );
      }
    }
  }

  Future<void> _confirmDelete(
    BuildContext context,
    WebhooksNotifier notifier,
  ) async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Delete webhook?'),
        content: Text('Remove "${_summary(hook.prompt)}"?'),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('Delete'),
          ),
        ],
      ),
    );
    if (ok == true) {
      try {
        await notifier.remove(hook.id);
      } catch (e) {
        if (context.mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(content: Text('Error: $e')),
          );
        }
      }
    }
  }

  static String _summary(String s) {
    if (s.length <= 40) return s;
    return '${s.substring(0, 40)}...';
  }
}
