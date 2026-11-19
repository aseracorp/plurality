import 'package:flutter/material.dart';

import '../../api/device_channel.dart';

/// Banner that makes an UNATTENDED device call visible.
///
/// A background run (cron, webhook, sub-agent) can now dispatch
/// `shell_client__exec` / `filesystem_client__fs_*` / MCP tools to this app
/// over the device channel (docs/device-channel.md) with no chat window
/// streaming that conversation. Nothing in the chat UI would otherwise show
/// it: the run lives in its own conversation row, and the tool result is
/// persisted there, so the user's machine does work while the app looks idle.
/// That is the invisible-mutation problem, and the banner is the answer.
///
/// Deliberately informational, not a gate: the server dispatches device calls
/// in place and blocks its LLM loop on the result, so refusing a call from a
/// banner would hang a background run. The channel executes, publishes, and
/// shows; the dismiss button only clears the log.
///
/// Rendered from [DeviceChannel.activityChanged], a ValueNotifier — the same
/// observation pattern the sidebar uses for `conversationStatuses`, so no new
/// state system is introduced.
class DeviceActivityBanner extends StatelessWidget {
  final DeviceChannel channel;
  final VoidCallback? onDismiss;

  const DeviceActivityBanner({
    super.key,
    required this.channel,
    this.onDismiss,
  });

  @override
  Widget build(BuildContext context) {
    return ValueListenableBuilder<List<DeviceActivityEntry>>(
      valueListenable: channel.activityChanged,
      builder: (context, entries, _) {
        if (entries.isEmpty) return const SizedBox.shrink();
        final newest = entries.first;
        final scheme = Theme.of(context).colorScheme;
        final runningCount = entries.where((e) => e.isRunning).length;

        return Container(
          margin: const EdgeInsets.symmetric(horizontal: 12, vertical: 4),
          padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 4),
          decoration: BoxDecoration(
            color: scheme.secondaryContainer,
            borderRadius: BorderRadius.circular(5),
            border: Border.all(color: scheme.outline),
          ),
          child: Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              if (runningCount > 0)
                SizedBox(
                  width: 12,
                  height: 12,
                  child: CircularProgressIndicator(
                    strokeWidth: 1.5,
                    color: scheme.primary,
                  ),
                )
              else
                Icon(Icons.devices_other, size: 14, color: scheme.onSecondaryContainer),
              const SizedBox(width: 6),
              Flexible(
                child: Tooltip(
                  // Full log: which calls ran, which were refused and why.
                  message: entries.map((e) => e.describe()).join('\n'),
                  child: Text(
                    entries.length == 1
                        ? newest.describe()
                        : '${newest.describe()} (+${entries.length - 1} more)',
                    style: TextStyle(
                      color: scheme.onSecondaryContainer,
                      fontSize: 12,
                    ),
                    overflow: TextOverflow.ellipsis,
                    maxLines: 1,
                  ),
                ),
              ),
              const SizedBox(width: 4),
              Tooltip(
                message: 'Hide the device-activity log. Background runs can '
                    'still use this device; the channel keeps running.',
                child: IconButton(
                  icon: Icon(Icons.close, size: 14, color: scheme.onSecondaryContainer),
                  visualDensity: VisualDensity.compact,
                  constraints: const BoxConstraints(minWidth: 24, minHeight: 24),
                  padding: EdgeInsets.zero,
                  onPressed: () {
                    channel.clearActivity();
                    onDismiss?.call();
                  },
                ),
              ),
            ],
          ),
        );
      },
    );
  }
}

/// One-line label for a device call, for snackbars and status rows.
String describeDeviceEntry(DeviceActivityEntry entry) => entry.describe();
