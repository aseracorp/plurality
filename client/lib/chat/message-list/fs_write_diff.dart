import 'dart:convert';
import 'package:flutter/material.dart';

/// Tools whose arguments should render as a git-diff-style preview rather
/// than a raw JSON dump. Both server and device variants share the same
/// arg shape (op + path + old_text/new_text/content/dest_path).
const fsWriteToolNames = {
  'fs_write',
  'filesystem_server__fs_write',
  'filesystem_client__fs_write',
};

/// Returns a diff-style preview of an fs_write tool call's arguments, or null
/// if [toolName] is not an fs_write tool. The widget caps its own height and
/// scrolls internally so it never takes over the surrounding container.
///
/// [maxHeight] caps the inner scroll area. The header / path strip sits
/// outside that cap.
Widget? buildFsWriteDiff({
  required String toolName,
  required String argumentsJson,
  required BuildContext context,
  double maxHeight = 320,
}) {
  if (!fsWriteToolNames.contains(toolName)) return null;
  Map<String, dynamic> args;
  try {
    args = Map<String, dynamic>.from(
      jsonDecode(argumentsJson.isEmpty ? '{}' : argumentsJson) as Map,
    );
  } catch (_) {
    return null;
  }

  var op = (args['op'] as String?) ?? '';
  final path = (args['path'] as String?) ?? '';

  // LLMs occasionally drop the 'op' arg — infer it from which payload
  // fields are present so we can still render a useful diff.
  if (op.isEmpty) {
    final hasOld = (args['old_text'] as String?)?.isNotEmpty == true;
    final hasNew = (args['new_text'] as String?)?.isNotEmpty == true;
    final hasContent = (args['content'] as String?) != null;
    final hasDest = (args['dest_path'] as String?)?.isNotEmpty == true;
    if (hasOld || hasNew) {
      op = 'edit';
    } else if (hasContent) {
      op = 'create';
    } else if (hasDest) {
      op = 'move';
    }
  }

  switch (op) {
    case 'edit':
      return _DiffPreview(
        op: op,
        path: path,
        oldText: (args['old_text'] as String?) ?? '',
        newText: (args['new_text'] as String?) ?? '',
        maxHeight: maxHeight,
      );
    case 'create':
      return _DiffPreview(
        op: op,
        path: path,
        oldText: null,
        newText: (args['content'] as String?) ?? '',
        maxHeight: maxHeight,
      );
    case 'delete':
      return _DiffPreview(
        op: op,
        path: path,
        oldText: '(file contents will be removed)',
        newText: null,
        maxHeight: maxHeight,
      );
    case 'mkdir':
      return _SimpleOpRow(op: 'mkdir', path: path, color: Colors.green);
    case 'copy':
    case 'move':
      final dest = (args['dest_path'] as String?) ?? '';
      return _SimpleOpRow(op: op, path: '$path → $dest', color: Colors.blue);
    default:
      return null;
  }
}

class _DiffPreview extends StatelessWidget {
  final String op;
  final String path;
  final String? oldText; // null = nothing on the "before" side (create)
  final String? newText; // null = nothing on the "after" side (delete)
  final double maxHeight;

  const _DiffPreview({
    required this.op,
    required this.path,
    required this.oldText,
    required this.newText,
    required this.maxHeight,
  });

  @override
  Widget build(BuildContext context) {
    final isDark = Theme.of(context).brightness == Brightness.dark;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      mainAxisSize: MainAxisSize.min,
      children: [
        _PathHeader(op: op, path: path),
        const SizedBox(height: 6),
        ConstrainedBox(
          constraints: BoxConstraints(maxHeight: maxHeight),
          child: ClipRRect(
            borderRadius: BorderRadius.circular(4),
            child: SingleChildScrollView(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  if (oldText != null)
                    _DiffBlock(
                      text: oldText!,
                      sign: '-',
                      background: Colors.red.withOpacity(isDark ? 0.18 : 0.10),
                      border: Colors.red.withOpacity(0.4),
                      fg: isDark ? Colors.red[200]! : Colors.red[900]!,
                    ),
                  if (oldText != null && newText != null)
                    const SizedBox(height: 4),
                  if (newText != null)
                    _DiffBlock(
                      text: newText!,
                      sign: '+',
                      background: Colors.green.withOpacity(isDark ? 0.18 : 0.10),
                      border: Colors.green.withOpacity(0.4),
                      fg: isDark ? Colors.green[200]! : Colors.green[900]!,
                    ),
                ],
              ),
            ),
          ),
        ),
      ],
    );
  }
}

class _PathHeader extends StatelessWidget {
  final String op;
  final String path;
  const _PathHeader({required this.op, required this.path});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
      decoration: BoxDecoration(
        color: Theme.of(context)
            .colorScheme
            .surfaceContainerHighest
            .withOpacity(0.6),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        children: [
          Text(
            op,
            style: TextStyle(
              fontFamily: 'monospace',
              fontSize: 11,
              fontWeight: FontWeight.bold,
              color: Theme.of(context).colorScheme.onSurfaceVariant,
            ),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: Text(
              path,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(
                fontFamily: 'monospace',
                fontSize: 12,
                color: Theme.of(context).colorScheme.onSurface,
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _DiffBlock extends StatelessWidget {
  final String text;
  final String sign;
  final Color background;
  final Color border;
  final Color fg;

  const _DiffBlock({
    required this.text,
    required this.sign,
    required this.background,
    required this.border,
    required this.fg,
  });

  @override
  Widget build(BuildContext context) {
    final lines = text.split('\n');
    return Container(
      decoration: BoxDecoration(
        color: background,
        border: Border.all(color: border),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: lines
            .map(
              (line) => Padding(
                padding:
                    const EdgeInsets.symmetric(horizontal: 6, vertical: 1),
                child: SelectableText.rich(
                  TextSpan(
                    style: TextStyle(
                      fontFamily: 'monospace',
                      fontSize: 12,
                      color: fg,
                    ),
                    children: [
                      TextSpan(
                        text: '$sign ',
                        style: const TextStyle(fontWeight: FontWeight.bold),
                      ),
                      TextSpan(text: line),
                    ],
                  ),
                ),
              ),
            )
            .toList(),
      ),
    );
  }
}

class _SimpleOpRow extends StatelessWidget {
  final String op;
  final String path;
  final Color color;

  const _SimpleOpRow({
    required this.op,
    required this.path,
    required this.color,
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 6),
      decoration: BoxDecoration(
        color: color.withOpacity(0.10),
        border: Border.all(color: color.withOpacity(0.4)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        children: [
          Text(
            op,
            style: TextStyle(
              fontFamily: 'monospace',
              fontSize: 11,
              fontWeight: FontWeight.bold,
              color: color,
            ),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: SelectableText(
              path,
              style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
            ),
          ),
        ],
      ),
    );
  }
}
