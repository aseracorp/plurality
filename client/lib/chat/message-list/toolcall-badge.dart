import 'package:flutter/material.dart';
import '../../utils/types.dart';
import '../../utils/index.dart' show formatToolDisplayName;
import 'dart:convert';
import 'fs_write_diff.dart';
import 'fs_read_attach.dart';

class ToolCallBadge extends StatelessWidget {
  final ToolCall toolCall;
  final VoidCallback? onTap;
  final bool isLoading;
  final Message? resultMessage;

  const ToolCallBadge({
    Key? key,
    required this.toolCall,
    this.onTap,
    this.isLoading = false,
    this.resultMessage,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    String loadingString = toolCall.loading.isNotEmpty
        ? toolCall.loading
        : formatToolDisplayName(toolCall.function.name);

    // Substitute {{placeholders}} with actual argument values
    try {
      var args = Map<String, dynamic>.from(jsonDecode(toolCall.function.arguments));
      args.forEach((key, value) {
        if (value is! String) value = value.toString();
        loadingString = loadingString.replaceAll('{{$key}}', value);
      });
    } catch (_) {}

    // Remove any remaining unresolved {{placeholders}}
    loadingString = loadingString
        .replaceAll(RegExp(r'\{\{.*?\}\}'), '')
        .replaceAll('  ', ' ')
        .trim();

    final inlineDiff = buildFsWriteDiff(
      toolName: toolCall.function.name,
      argumentsJson: toolCall.function.arguments,
      context: context,
      maxHeight: 220,
    );

    final inlineReadAttach = buildFsReadAttach(
      toolName: toolCall.function.name,
      argumentsJson: toolCall.function.arguments,
      resultMessage: resultMessage,
      context: context,
    );

    final chip = Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
      decoration: BoxDecoration(
        color: Theme.of(context).colorScheme.surfaceContainerHighest,
        borderRadius: BorderRadius.circular(16),
        border: Border.all(
          color: Theme.of(context).colorScheme.outline.withValues(alpha: 0.5),
        ),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          // Icon — from base64 if available, else fallback
          if (toolCall.iconURL.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(right: 8),
              child: SizedBox(
                width: 18,
                height: 18,
                child: Image.memory(
                  base64Decode(toolCall.iconURL),
                  width: 18,
                  fit: BoxFit.cover,
                  cacheWidth: 18,
                  gaplessPlayback: true,
                ),
              ),
            )
          else
            Padding(
              padding: const EdgeInsets.only(right: 8),
              child: Icon(
                Icons.extension,
                size: 16,
                color: Theme.of(context).colorScheme.onSurfaceVariant,
              ),
            ),

          // Tool display text
          Flexible(
            child: Text(
              loadingString.isEmpty ? formatToolDisplayName(toolCall.function.name) : loadingString,
              overflow: TextOverflow.ellipsis,
              maxLines: 1,
              style: TextStyle(
                color: Theme.of(context).colorScheme.onSurfaceVariant,
                fontWeight: FontWeight.w500,
                fontSize: 14,
              ),
            ),
          ),

          // Loading indicator
          if (isLoading)
            Padding(
              padding: const EdgeInsets.only(left: 8),
              child: SizedBox(
                width: 12,
                height: 12,
                child: CircularProgressIndicator(
                  strokeWidth: 2,
                  color: Theme.of(context).colorScheme.onSurfaceVariant,
                ),
              ),
            ),
        ],
      ),
    );

    return GestureDetector(
      onTap: () => _showPreviewModal(context),
      child: Container(
        margin: const EdgeInsets.only(bottom: 8),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: [
            Align(alignment: Alignment.centerLeft, child: chip),
            if (inlineDiff != null) ...[
              const SizedBox(height: 6),
              FractionallySizedBox(
                widthFactor: 0.5,
                alignment: Alignment.centerLeft,
                child: inlineDiff,
              ),
            ],
            if (inlineReadAttach != null) ...[
              const SizedBox(height: 6),
              FractionallySizedBox(
                widthFactor: 0.5,
                alignment: Alignment.centerLeft,
                child: inlineReadAttach,
              ),
            ],
          ],
        ),
      ),
    );
  }

  Widget _buildPreviewBody(BuildContext context, bool isDarkMode) {
    final diff = buildFsWriteDiff(
      toolName: toolCall.function.name,
      argumentsJson: toolCall.function.arguments,
      context: context,
      maxHeight: MediaQuery.of(context).size.height * 0.45,
    );

    if (diff != null) {
      return Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          diff,
          const SizedBox(height: 12),
          Text(
            'Result',
            style: TextStyle(
              fontWeight: FontWeight.bold,
              color: isDarkMode ? Colors.white70 : Colors.black54,
            ),
          ),
          const SizedBox(height: 4),
          SelectableText(
            resultMessage?.textContent ?? '(pending...)',
            style: TextStyle(
              fontSize: 13.0,
              fontFamily: 'monospace',
              color: isDarkMode ? Colors.white : Colors.black,
            ),
          ),
        ],
      );
    }

    return SelectableText(
      "Tool: ${formatToolDisplayName(toolCall.function.name)}\n"
      "Arguments: ${toolCall.function.arguments}\n"
      "------------------\n"
      "${resultMessage?.textContent ?? '(pending...)'}",
      style: TextStyle(
        fontSize: 14.0,
        color: isDarkMode ? Colors.white : Colors.black,
      ),
    );
  }

  void _showPreviewModal(BuildContext context) {
    showDialog(
      context: context,
      builder: (BuildContext context) {
        final isDarkMode = Theme.of(context).brightness == Brightness.dark;

        return Dialog(
          insetPadding: const EdgeInsets.all(16.0),
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(8.0),
          ),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Container(
                padding: const EdgeInsets.symmetric(vertical: 8, horizontal: 8),
                decoration: BoxDecoration(
                  color: isDarkMode ? Colors.grey[800] : Colors.grey[100],
                  borderRadius: const BorderRadius.only(
                    topLeft: Radius.circular(8.0),
                    topRight: Radius.circular(8.0),
                  ),
                ),
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.spaceBetween,
                  children: [
                    IconButton(
                      icon: const Icon(Icons.close),
                      onPressed: () => Navigator.of(context).pop(),
                      padding: EdgeInsets.zero,
                      constraints: const BoxConstraints(),
                      iconSize: 20,
                    ),
                    Text(
                      formatToolDisplayName(toolCall.function.name),
                      style: const TextStyle(fontWeight: FontWeight.bold),
                    ),
                    const SizedBox(width: 28),
                  ],
                ),
              ),
              Container(
                decoration: BoxDecoration(
                  color: isDarkMode ? Colors.grey[900] : Colors.white,
                  borderRadius: const BorderRadius.only(
                    bottomLeft: Radius.circular(8.0),
                    bottomRight: Radius.circular(8.0),
                  ),
                ),
                constraints: BoxConstraints(
                  maxHeight: MediaQuery.of(context).size.height * 0.75,
                ),
                padding: const EdgeInsets.all(16.0),
                child: SingleChildScrollView(
                  child: _buildPreviewBody(context, isDarkMode),
                ),
              ),
            ],
          ),
        );
      },
    );
  }
}
