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

  return _LongTaskChecklist(
    tasks: tasks,
    // hasNudge is true when the server appended a reminder to this snapshot.
    // The raw nudge text is the full LLM prompt and isn't user-facing — we
    // show our own short summary instead.
    hasNudge: (state['nudge'] as String?)?.isNotEmpty ?? false,
    paused: state['paused'] == true,
    pauseReason: state['pause_reason'] as String?,
  );
}

class _LongTaskChecklist extends StatelessWidget {
  final List<Map<String, dynamic>> tasks;
  final bool hasNudge;
  final bool paused;
  final String? pauseReason;

  const _LongTaskChecklist({
    required this.tasks,
    this.hasNudge = false,
    this.paused = false,
    this.pauseReason,
  });

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final doneCount = tasks.where((t) => t['done'] == true).length;
    final total = tasks.length;
    final openCount = total - doneCount;

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
              child: Row(
                children: [
                  Text(
                    'Tasks  $doneCount / $total',
                    style: TextStyle(
                      fontSize: 12,
                      fontWeight: FontWeight.w600,
                      color: scheme.onSurfaceVariant,
                    ),
                  ),
                  if (paused) ...[
                    const SizedBox(width: 6),
                    Icon(Icons.pause_circle, size: 14, color: scheme.tertiary),
                    const SizedBox(width: 2),
                    Text(
                      'Paused',
                      style: TextStyle(
                        fontSize: 11,
                        fontWeight: FontWeight.w600,
                        color: scheme.tertiary,
                      ),
                    ),
                  ],
                ],
              ),
            ),
          if (paused && pauseReason != null && pauseReason!.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(bottom: 6),
              child: Text(
                pauseReason!,
                style: TextStyle(
                  fontSize: 12,
                  fontStyle: FontStyle.italic,
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
          if (hasNudge && openCount > 0)
            Padding(
              padding: const EdgeInsets.only(top: 6),
              child: Text(
                openCount == 1
                    ? 'The assistant still has 1 outstanding task.'
                    : 'The assistant still has $openCount outstanding tasks.',
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
