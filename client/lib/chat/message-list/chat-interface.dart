
/// Self-contained approval banner widget with its own local state.
/// State is destroyed when the widget unmounts (conversation changes, approval submitted).
class ToolApprovalBanner extends StatefulWidget {
  final List<ToolCall> askTools;
  final ToolCall Function(ToolCall) enrichToolCall;
  final void Function(Map<String, bool> decisions) onSubmit;

  const ToolApprovalBanner({
    super.key,
    required this.askTools,
    required this.enrichToolCall,
    required this.onSubmit,
  });

  @override
  State<ToolApprovalBanner> createState() => _ToolApprovalBannerState();
}

class _ToolApprovalBannerState extends State<ToolApprovalBanner> {
  final Map<String, bool> _decisions = {};

  /// Per-tool scroll controllers so each args strip has its own Scrollbar.
  /// Lazily created in [_argsScrollController] and disposed on unmount.
  final Map<String, ScrollController> _argsControllers = {};

  ScrollController _argsScrollController(String toolCallId) {
    return _argsControllers.putIfAbsent(toolCallId, () => ScrollController());
  }

  @override
  void dispose() {
    for (final c in _argsControllers.values) {
      c.dispose();
    }
    super.dispose();
  }

  String _formatArgs(String argsJson) {
    try {
      final args = Map<String, dynamic>.from(jsonDecode(argsJson));
      if (args.isEmpty) return '';
      return args.entries.map((e) => '${e.key}: ${e.value}').join('\n');
    } catch (_) {
      return argsJson;
    }
  }

  String _displayLabel(ToolCall tc) {
    final enriched = widget.enrichToolCall(tc);
    String label = enriched.loading.isNotEmpty ? enriched.loading : formatToolDisplayName(enriched.function.name);
    try {
      final args = Map<String, dynamic>.from(jsonDecode(tc.function.arguments));
      args.forEach((key, value) {
        label = label.replaceAll('{{$key}}', value.toString());
      });
    } catch (_) {}
    return label.replaceAll(RegExp(r'\{\{.*?\}\}'), '').replaceAll('  ', ' ').trim();
  }

  void _decide(String toolCallId, bool approved) {
    setState(() => _decisions[toolCallId] = approved);
    if (widget.askTools.every((tc) => _decisions.containsKey(tc.id))) {
      widget.onSubmit(Map<String, bool>.from(_decisions));
    }
  }

  void _decideAll(bool approved) {
    for (final tc in widget.askTools) {
      _decisions[tc.id] = approved;
    }
    widget.onSubmit(Map<String, bool>.from(_decisions));
  }

  @override
  Widget build(BuildContext context) {
    final isDark = Theme.of(context).brightness == Brightness.dark;
    final accentColor = Theme.of(context).colorScheme.primary;

    return FractionallySizedBox(
      widthFactor: 0.5,
      alignment: Alignment.centerLeft,
      child: Container(
        margin: const EdgeInsets.symmetric(vertical: 8),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(12),
          border: Border.all(color: accentColor.withOpacity(0.4)),
          color: accentColor.withOpacity(isDark ? 0.08 : 0.04),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 12, 16, 8),
              child: Row(
                children: [
                  Icon(Icons.shield_outlined, size: 18, color: accentColor),
                  const SizedBox(width: 8),
                  Text(
                    'Approval required',
                    style: TextStyle(
                      fontWeight: FontWeight.w600,
                      fontSize: 14,
                      color: Theme.of(context).colorScheme.onSurface,
                    ),
                  ),
                ],
              ),
            ),
            // Tool cards
            ...widget.askTools.map((tc) {
              final label = _displayLabel(tc);
              final argsFormatted = _formatArgs(tc.function.arguments);
              final enriched = widget.enrichToolCall(tc);
              final decision = _decisions[tc.id];

              return Padding(
                padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 4),
                child: Container(
                  width: double.infinity,
                  padding: const EdgeInsets.all(10),
                  decoration: BoxDecoration(
                    color: decision == null
                        ? Theme.of(context).colorScheme.surface
                        : decision
                            ? Colors.green.withOpacity(isDark ? 0.1 : 0.05)
                            : Colors.red.withOpacity(isDark ? 0.1 : 0.05),
                    borderRadius: BorderRadius.circular(8),
                    border: Border.all(
                      color: decision == null
                          ? Theme.of(context).colorScheme.outline.withOpacity(0.2)
                          : decision
                              ? Colors.green.withOpacity(0.4)
                              : Colors.red.withOpacity(0.4),
                    ),
                  ),
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Row(
                        children: [
                          if (enriched.iconURL.isNotEmpty)
                            Padding(
                              padding: const EdgeInsets.only(right: 8),
                              child: SizedBox(
                                width: 16, height: 16,
                                child: Image.memory(
                                  base64Decode(enriched.iconURL),
                                  width: 16, fit: BoxFit.cover, cacheWidth: 16,
                                  gaplessPlayback: true,
                                ),
                              ),
                            )
                          else
                            Padding(
                              padding: const EdgeInsets.only(right: 8),
                              child: Icon(Icons.extension, size: 16,
                                color: Theme.of(context).colorScheme.onSurfaceVariant),
                            ),
                          Expanded(
                            child: Text(
                              label,
                              style: TextStyle(
                                fontWeight: FontWeight.w500,
                                fontSize: 13,
                                color: Theme.of(context).colorScheme.onSurface,
                              ),
                            ),
                          ),
                          if (decision == null) ...[
                            OutlinedButton(
                              onPressed: () => _decide(tc.id, false),
                              style: OutlinedButton.styleFrom(
                                foregroundColor: Theme.of(context).colorScheme.error,
                                side: BorderSide(color: Theme.of(context).colorScheme.error.withOpacity(0.5)),
                                padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                                minimumSize: const Size(0, 36),
                              ),
                              child: const Text('Deny'),
                            ),
                            const SizedBox(width: 6),
                            FilledButton(
                              onPressed: () => _decide(tc.id, true),
                              style: FilledButton.styleFrom(
                                backgroundColor: Colors.green,
                                padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                                minimumSize: const Size(0, 36),
                              ),
                              child: const Text('Approve'),
                            ),
                          ] else
                            Icon(
                              decision ? Icons.check_circle : Icons.cancel,
                              size: 20,
                              color: decision ? Colors.green : Theme.of(context).colorScheme.error,
                            ),
                        ],
                      ),
                      if (() {
                        final diff = buildFsWriteDiff(
                          toolName: tc.function.name,
                          argumentsJson: tc.function.arguments,
                          context: context,
                          maxHeight: 220,
                        );
                        return diff != null;
                      }()) ...[
                        const SizedBox(height: 6),
                        Builder(builder: (context) {
                          final diff = buildFsWriteDiff(
                            toolName: tc.function.name,
                            argumentsJson: tc.function.arguments,
                            context: context,
                            maxHeight: 220,
                          )!;
                          return diff;
                        }),
                      ] else if (argsFormatted.isNotEmpty) ...[
                        const SizedBox(height: 6),
                        ConstrainedBox(
                          constraints: const BoxConstraints(maxHeight: 70),
                          child: Scrollbar(
                            controller: _argsScrollController(tc.id),
                            thumbVisibility: true,
                            thickness: 4,
                            radius: const Radius.circular(2),
                            child: SingleChildScrollView(
                              controller: _argsScrollController(tc.id),
                              child: Container(
                                width: double.infinity,
                                padding: const EdgeInsets.all(8),
                                decoration: BoxDecoration(
                                  color: Theme.of(context).colorScheme.surfaceContainerHighest.withOpacity(0.5),
                                  borderRadius: BorderRadius.circular(6),
                                ),
                                child: Text(
                                  argsFormatted,
                                  style: TextStyle(
                                    fontSize: 12,
                                    fontFamily: 'monospace',
                                    color: Theme.of(context).colorScheme.onSurfaceVariant,
                                  ),
                                ),
                              ),
                            ),
                          ),
                        ),
                      ],
                    ],
                  ),
                ),
              );
            }),
            // Bulk buttons (only for multiple tools)
            if (widget.askTools.length > 1)
              Padding(
                padding: const EdgeInsets.fromLTRB(12, 8, 12, 12),
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.end,
                  children: [
                    TextButton(
                      onPressed: () => _decideAll(false),
                      child: const Text('Deny All'),
                    ),
                    TextButton(
                      onPressed: () => _decideAll(true),
                      child: const Text('Approve All'),
                    ),
                  ],
                ),
              ),
          ],
        ),
      ),
    );
  }
}
