import 'dart:convert';
import 'package:flutter/material.dart';
import '../../utils/types.dart';

const longTaskToolName = 'long_task';

/// Returns a checklist preview of the current long_task state, or null when
/// [toolName] isn't long_task or the result message isn't parseable yet.
///
/// Renders from the result message because the args of any individual call
/// only describe the diff (e.g. operation:'complete', ids:['t2']) — the
/// result body carries the full authoritative state.
Widget? buildLongTaskChecklist({
  required String toolName,
  required Message? resultMessage,
  required BuildContext context,
}) {
  if (toolName != longTaskToolName) return null;
  if (resultMessage == null) return null;

  final raw = resultMessage.textContent;
  if (raw.isEmpty) return null;

  Map<String, dynamic> state;
  try {
    final decoded = jsonDecode(raw);
    if (decoded is! Map) return null;
    state = Map<String, dynamic>.from(decoded);
  } catch (_) {
    // Error strings from the tool start with "Error: " and aren't JSON.
    return null;
  }

  final tasksRaw = state['tasks'];
  if (tasksRaw is! List) return null;

  final tasks =
      tasksRaw
          .whereType<Map>()
          .map((m) => Map<String, dynamic>.from(m))
          .toList();

  return _LongTaskChecklist(tasks: tasks, nudge: state['nudge'] as String?);
}

class _LongTaskChecklist extends StatelessWidget {
  final List<Map<String, dynamic>> tasks;
  final String? nudge;

  const _LongTaskChecklist({required this.tasks, this.nudge});

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final doneCount = tasks.where((t) => t['done'] == true).length;
    final total = tasks.length;

    return Container(
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
        color: scheme.surfaceContainerHighest.withValues(alpha: 0.7),
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: scheme.outline.withValues(alpha: 0.4)),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisSize: MainAxisSize.min,
        children: [
          if (total > 0)
            Padding(
              padding: const EdgeInsets.only(bottom: 6),
              child: Text(
                'Tasks  $doneCount / $total',
                style: TextStyle(
                  fontSize: 12,
                  fontWeight: FontWeight.w600,
                  color: scheme.onSurfaceVariant,
                ),
              ),
            ),
          if (total == 0)
            Text(
              '(no tasks)',
              style: TextStyle(
                fontSize: 13,
                fontStyle: FontStyle.italic,
                color: scheme.onSurfaceVariant,
              ),
            ),
          ...tasks.map(
            (t) => _TaskRow(
              title: (t['title'] as String?) ?? '',
              done: t['done'] == true,
            ),
          ),
          if (nudge != null && nudge!.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(top: 6),
              child: Text(
                nudge!,
                style: TextStyle(
                  fontSize: 12,
                  fontStyle: FontStyle.italic,
                  color: scheme.tertiary,
                ),
              ),
            ),
        ],
      ),
    );
  }
}

class _TaskRow extends StatelessWidget {
  final String title;
  final bool done;

  const _TaskRow({required this.title, required this.done});

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 2),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(
            done ? Icons.check_box : Icons.check_box_outline_blank,
            size: 16,
            color: done ? scheme.primary : scheme.onSurfaceVariant,
          ),
          const SizedBox(width: 6),
          Expanded(
            child: Text(
              title,
              style: TextStyle(
                fontSize: 13,
                decoration: done ? TextDecoration.lineThrough : null,
                color:
                    done
                        ? scheme.onSurface.withValues(alpha: 0.55)
                        : scheme.onSurface,
              ),
            ),
          ),
        ],
      ),
    );
  }
}
