import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/cron-service.dart';
import '../api/mini-apps.dart';
import '../utils/types.dart';
import '../widgets/preset_dropdown.dart';

const _defaultPresetId = 'default-background-agent';

class CronListScreen extends ConsumerStatefulWidget {
  const CronListScreen({super.key});

  @override
  ConsumerState<CronListScreen> createState() => _CronListScreenState();
}

class _CronListScreenState extends ConsumerState<CronListScreen> {
  Future<List<MiniApp>>? _presetsFuture;

  @override
  void initState() {
    super.initState();
    _presetsFuture = MiniAppsService().getAllMiniApps();
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted) ref.read(cronsProvider.notifier).refresh();
    });
  }

  @override
  Widget build(BuildContext context) {
    final crons = ref.watch(cronsProvider);

    return Scaffold(
      appBar: AppBar(title: const Text('Schedules')),
      body: RefreshIndicator(
        onRefresh: () => ref.read(cronsProvider.notifier).refresh(),
        child:
            crons.isEmpty
                ? ListView(
                  children: const [
                    SizedBox(height: 96),
                    Center(
                      child: Padding(
                        padding: EdgeInsets.all(24),
                        child: Text(
                          'No schedules yet.\nTap + to add one.',
                          textAlign: TextAlign.center,
                          style: TextStyle(fontSize: 16),
                        ),
                      ),
                    ),
                  ],
                )
                : ListView.separated(
                  itemCount: crons.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (context, i) {
                    final job = crons[i];
                    return _CronTile(
                      job: job,
                      presetsFuture: _presetsFuture,
                      onEdit: () => _openDialog(existing: job),
                    );
                  },
                ),
      ),
      floatingActionButton: FloatingActionButton(
        onPressed: () => _openDialog(),
        child: const Icon(Icons.add),
      ),
    );
  }

  Future<void> _openDialog({CronJob? existing}) async {
    final presets = await _presetsFuture ?? <MiniApp>[];
    if (!mounted) return;

    final scheduleCtrl = TextEditingController(text: existing?.schedule ?? '');
    final promptCtrl = TextEditingController(text: existing?.prompt ?? '');
    String selectedPreset = existing?.presetId.isNotEmpty == true
        ? existing!.presetId
        : _defaultPresetId;
    final bool originalAppend = existing?.conversationId.isNotEmpty ?? false;
    bool appendToSame = originalAppend;

    final isPresetInList = presets.any((p) => p.id == selectedPreset);
    if (!isPresetInList && presets.isNotEmpty) {
      selectedPreset = presets.first.id;
    }

    final saved = await showDialog<bool>(
      context: context,
      builder: (ctx) {
        return StatefulBuilder(
          builder: (ctx, setLocal) {
            return AlertDialog(
              title: Text(existing == null ? 'New Schedule' : 'Edit Schedule'),
              content: SingleChildScrollView(
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    TextField(
                      controller: scheduleCtrl,
                      decoration: const InputDecoration(
                        labelText: 'Schedule',
                        hintText: '0 9 * * *',
                      ),
                      style: const TextStyle(fontFamily: 'monospace'),
                    ),
                    const Padding(
                      padding: EdgeInsets.only(top: 4, bottom: 12),
                      child: Text(
                        'm h dom mon dow',
                        style: TextStyle(fontSize: 12, color: Colors.grey),
                      ),
                    ),
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
                    const SizedBox(height: 8),
                    CheckboxListTile(
                      contentPadding: EdgeInsets.zero,
                      controlAffinity: ListTileControlAffinity.leading,
                      title: const Text('Append to the same conversation'),
                      subtitle: const Text(
                        'Every firing adds to one shared conversation instead of starting a new one.',
                        style: TextStyle(fontSize: 12),
                      ),
                      value: appendToSame,
                      onChanged: (v) =>
                          setLocal(() => appendToSame = v ?? false),
                    ),
                  ],
                ),
              ),
              actions: [
                TextButton(
                  onPressed: () => Navigator.of(ctx).pop(false),
                  child: const Text('Cancel'),
                ),
                FilledButton(
                  onPressed: () async {
                    final schedule = scheduleCtrl.text.trim();
                    final prompt = promptCtrl.text.trim();
                    if (schedule.isEmpty || prompt.isEmpty) {
                      ScaffoldMessenger.of(ctx).showSnackBar(
                        const SnackBar(
                          content: Text('Schedule and prompt are required'),
                        ),
                      );
                      return;
                    }
                    try {
                      final notifier = ref.read(cronsProvider.notifier);
                      if (existing == null) {
                        await notifier.add(
                          schedule: schedule,
                          prompt: prompt,
                          presetId: selectedPreset,
                          conversationId: appendToSame ? '1' : '',
                        );
                      } else {
                        // Only resend conversation_id when the checkbox state
                        // changed — otherwise we'd clobber the persisted real
                        // conversation_id with the "1" sentinel on every save.
                        String? convPatch;
                        if (appendToSame != originalAppend) {
                          convPatch = appendToSame ? '1' : '';
                        }
                        await notifier.update(
                          existing.id,
                          schedule: schedule,
                          prompt: prompt,
                          presetId: selectedPreset,
                          conversationId: convPatch,
                        );
                      }
                      if (ctx.mounted) Navigator.of(ctx).pop(true);
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

    scheduleCtrl.dispose();
    promptCtrl.dispose();
    if (saved == true) {
      // already optimistic in notifier; nothing else to do
    }
  }
}

class _CronTile extends ConsumerWidget {
  final CronJob job;
  final Future<List<MiniApp>>? presetsFuture;
  final VoidCallback onEdit;

  const _CronTile({
    required this.job,
    required this.presetsFuture,
    required this.onEdit,
  });

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final notifier = ref.read(cronsProvider.notifier);

    return ListTile(
      isThreeLine: true,
      title: Text(
        job.prompt,
        maxLines: 2,
        overflow: TextOverflow.ellipsis,
        style: TextStyle(
          color: job.enabled ? null : Theme.of(context).disabledColor,
        ),
      ),
      subtitle: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            job.schedule,
            style: const TextStyle(fontFamily: 'monospace', fontSize: 13),
          ),
          FutureBuilder<List<MiniApp>>(
            future: presetsFuture,
            builder: (context, snap) {
              final name =
                  snap.data
                      ?.where((p) => p.id == job.presetId)
                      .map((p) => p.name)
                      .firstOrNull ??
                  job.presetId;
              return Text(
                name,
                style: const TextStyle(fontSize: 12, color: Colors.grey),
              );
            },
          ),
        ],
      ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Switch(
            value: job.enabled,
            onChanged: (v) => notifier.update(job.id, enabled: v),
          ),
          IconButton(
            icon: const Icon(Icons.play_arrow),
            tooltip: 'Run now',
            onPressed: () async {
              try {
                await notifier.runNow(job.id);
                if (context.mounted) {
                  ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(content: Text('Cron triggered')),
                  );
                }
              } catch (e) {
                if (context.mounted) {
                  ScaffoldMessenger.of(context).showSnackBar(
                    SnackBar(content: Text('Error: $e')),
                  );
                }
              }
            },
          ),
          IconButton(
            icon: const Icon(Icons.edit),
            tooltip: 'Edit',
            onPressed: onEdit,
          ),
          IconButton(
            icon: const Icon(Icons.delete),
            tooltip: 'Delete',
            onPressed: () async {
              final ok = await showDialog<bool>(
                context: context,
                builder:
                    (ctx) => AlertDialog(
                      title: const Text('Delete schedule?'),
                      content: Text('Remove "${_summary(job.prompt)}"?'),
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
                  await notifier.remove(job.id);
                } catch (e) {
                  if (context.mounted) {
                    ScaffoldMessenger.of(context).showSnackBar(
                      SnackBar(content: Text('Error: $e')),
                    );
                  }
                }
              }
            },
          ),
        ],
      ),
    );
  }

  static String _summary(String s) {
    if (s.length <= 40) return s;
    return '${s.substring(0, 40)}...';
  }
}
