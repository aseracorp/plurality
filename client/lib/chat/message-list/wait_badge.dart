import 'dart:async';
import 'dart:convert';
import 'package:flutter/material.dart';
import '../../utils/types.dart';

const waitToolName = 'wait';

/// True for synthetic "Timer is done" tool calls injected by the server after
/// a wait elapses. They exist solely to feed the LLM a fresh tool result so it
/// resumes its task — hiding them keeps a single "Timer Ns" badge for the
/// original wait rather than two stacked entries for the same pause.
bool isHiddenWaitResume(ToolCall tc) {
  if (tc.function.name != waitToolName) return false;
  try {
    final args = jsonDecode(tc.function.arguments);
    return args is Map && args['_resume'] == true;
  } catch (_) {
    return false;
  }
}

/// True for an assistant message whose only payload is a hidden resume tool
/// call (no text, no other tool calls). Such messages produce an empty bubble
/// once the resume call is filtered out, so the whole message gets skipped.
bool isHiddenWaitResumeMessage(Message m) {
  if (m.role != 'assistant') return false;
  if (m.textContent.isNotEmpty) return false;
  final tcs = m.toolCalls;
  if (tcs == null || tcs.isEmpty) return false;
  return tcs.every(isHiddenWaitResume);
}

/// Returns a live "Waiting Xs" / "Timer Xs" label for a wait tool call, or
/// null when [toolName] isn't 'wait' or the result message isn't parseable.
///
/// Used in place of the static loading-string substitution so the chip text
/// ticks each second instead of freezing on the initial value. After the
/// countdown reaches zero the label names the timer ("Timer 10s") rather
/// than just saying it resumed.
Widget? buildWaitChipLabel({
  required String toolName,
  required Message? resultMessage,
  required TextStyle style,
}) {
  if (toolName != waitToolName) return null;
  if (resultMessage == null) return null;

  final raw = resultMessage.textContent;
  if (raw.isEmpty) return null;

  Map<String, dynamic> data;
  try {
    final decoded = jsonDecode(raw);
    if (decoded is! Map) return null;
    data = Map<String, dynamic>.from(decoded);
  } catch (_) {
    return null;
  }

  final wakeAtStr = data['wake_at'] as String?;
  final waitSeconds = (data['wait_seconds'] as num?)?.toInt() ?? 0;
  if (wakeAtStr == null || waitSeconds <= 0) return null;
  DateTime wakeAt;
  try {
    wakeAt = DateTime.parse(wakeAtStr).toLocal();
  } catch (_) {
    return null;
  }

  return _WaitChipLabel(
    wakeAt: wakeAt,
    totalSeconds: waitSeconds,
    style: style,
  );
}

class _WaitChipLabel extends StatefulWidget {
  final DateTime wakeAt;
  final int totalSeconds;
  final TextStyle style;

  const _WaitChipLabel({
    required this.wakeAt,
    required this.totalSeconds,
    required this.style,
  });

  @override
  State<_WaitChipLabel> createState() => _WaitChipLabelState();
}

class _WaitChipLabelState extends State<_WaitChipLabel> {
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _scheduleTick();
  }

  void _scheduleTick() {
    _timer?.cancel();
    if (widget.wakeAt.difference(DateTime.now()) <= Duration.zero) return;
    _timer = Timer(const Duration(seconds: 1), () {
      if (!mounted) return;
      setState(() {});
      _scheduleTick();
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final remaining = widget.wakeAt.difference(DateTime.now());
    final label = remaining <= Duration.zero
        ? 'Timer ${_formatRemainingShort(Duration(seconds: widget.totalSeconds))}'
        : 'Waiting ${_formatRemainingShort(remaining)}';
    return Text(
      label,
      style: widget.style,
      overflow: TextOverflow.ellipsis,
      maxLines: 1,
    );
  }
}

String _formatRemainingShort(Duration d) {
  final secs = d.inSeconds;
  final h = secs ~/ 3600;
  final m = (secs % 3600) ~/ 60;
  final s = secs % 60;
  if (h > 0) return '${h}h ${m}m ${s}s';
  if (m > 0) return '${m}m ${s}s';
  return '${s}s';
}

/// Returns a countdown timer preview of a wait tool call, or null when
/// [toolName] isn't 'wait' or the result message isn't parseable yet.
///
/// The result body is JSON with shape:
///   {"wait_seconds": 10, "wake_at": "2026-05-11T12:34:56Z", "status": "..."}
///
/// While the wake-up timestamp lies in the future the widget renders a live
/// countdown. Once it passes, the widget names the timer ("Timer 10s") so the
/// completed call is recognisable in the chat history.
Widget? buildWaitBadge({
  required String toolName,
  required Message? resultMessage,
  required BuildContext context,
}) {
  if (toolName != waitToolName) return null;
  if (resultMessage == null) return null;

  final raw = resultMessage.textContent;
  if (raw.isEmpty) return null;

  Map<String, dynamic> data;
  try {
    final decoded = jsonDecode(raw);
    if (decoded is! Map) return null;
    data = Map<String, dynamic>.from(decoded);
  } catch (_) {
    // Error strings from the tool start with "Error: " and aren't JSON.
    return null;
  }

  final wakeAtStr = data['wake_at'] as String?;
  final waitSeconds = (data['wait_seconds'] as num?)?.toInt() ?? 0;
  if (wakeAtStr == null || waitSeconds <= 0) return null;

  DateTime? wakeAt;
  try {
    wakeAt = DateTime.parse(wakeAtStr).toLocal();
  } catch (_) {
    return null;
  }

  return _WaitCountdown(wakeAt: wakeAt, totalSeconds: waitSeconds);
}

class _WaitCountdown extends StatefulWidget {
  final DateTime wakeAt;
  final int totalSeconds;

  const _WaitCountdown({required this.wakeAt, required this.totalSeconds});

  @override
  State<_WaitCountdown> createState() => _WaitCountdownState();
}

class _WaitCountdownState extends State<_WaitCountdown> {
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _scheduleTick();
  }

  void _scheduleTick() {
    _timer?.cancel();
    final remaining = widget.wakeAt.difference(DateTime.now());
    if (remaining <= Duration.zero) return;
    _timer = Timer(const Duration(seconds: 1), () {
      if (!mounted) return;
      setState(() {});
      _scheduleTick();
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  String _formatRemaining(Duration d) {
    if (d <= Duration.zero) return '0s';
    return _formatRemainingShort(d);
  }

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final now = DateTime.now();
    final remaining = widget.wakeAt.difference(now);
    final done = remaining <= Duration.zero;

    final total = widget.totalSeconds;
    final elapsed = total - remaining.inSeconds;
    final progress =
        done ? 1.0 : (elapsed.clamp(0, total)) / (total == 0 ? 1 : total);

    return Container(
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
        color: scheme.surfaceContainerHighest.withValues(alpha: 0.7),
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: scheme.outline.withValues(alpha: 0.4)),
      ),
      child: Row(
        children: [
          SizedBox(
            width: 32,
            height: 32,
            child: Stack(
              alignment: Alignment.center,
              children: [
                SizedBox(
                  width: 28,
                  height: 28,
                  child: CircularProgressIndicator(
                    value: progress,
                    strokeWidth: 3,
                    backgroundColor: scheme.outlineVariant.withValues(
                      alpha: 0.5,
                    ),
                    valueColor: AlwaysStoppedAnimation<Color>(
                      done ? scheme.primary : scheme.tertiary,
                    ),
                  ),
                ),
                Icon(
                  done ? Icons.check : Icons.schedule,
                  size: 14,
                  color: scheme.onSurfaceVariant,
                ),
              ],
            ),
          ),
          const SizedBox(width: 12),
          Expanded(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  done ? 'Timer ${_formatTotal(total)}' : 'Waiting…',
                  style: TextStyle(
                    fontSize: 12,
                    fontWeight: FontWeight.w600,
                    color: scheme.onSurfaceVariant,
                  ),
                ),
                const SizedBox(height: 2),
                Text(
                  done
                      ? 'Waited ${_formatTotal(total)}'
                      : '${_formatRemaining(remaining)} remaining (of ${_formatTotal(total)})',
                  style: TextStyle(
                    fontSize: 13,
                    fontFamily: 'monospace',
                    color: scheme.onSurface,
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  String _formatTotal(int totalSecs) =>
      _formatRemainingShort(Duration(seconds: totalSecs));
}
