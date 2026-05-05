import 'dart:convert';
import 'package:flutter/material.dart';
import '../../utils/types.dart';
import '../../utils/file-types.dart';
import 'attachments.dart';

/// Tools whose `read_attach` op should render an inline attachment-with-download
/// card under the badge. Server and device variants share the same arg shape.
const fsReadToolNames = {
  'fs_read',
  'filesystem_server__fs_read',
  'filesystem_client__fs_read',
};

/// Returns an inline preview of the attachment produced by an `fs_read` call
/// with `op=read_attach`, or null when the call is a different tool / op or no
/// attachment is present yet. Mirrors the structure of [buildFsWriteDiff]: a
/// path header on top, the actual content below.
Widget? buildFsReadAttach({
  required String toolName,
  required String argumentsJson,
  required Message? resultMessage,
  required BuildContext context,
}) {
  if (!fsReadToolNames.contains(toolName)) return null;

  Map<String, dynamic> args;
  try {
    args = Map<String, dynamic>.from(
      jsonDecode(argumentsJson.isEmpty ? '{}' : argumentsJson) as Map,
    );
  } catch (_) {
    return null;
  }

  if ((args['op'] as String?) != 'read_attach') return null;

  final path = (args['path'] as String?) ?? '';

  final attachments = _attachmentsFromMessage(resultMessage);

  return _ReadAttachPreview(path: path, attachments: attachments);
}

/// Returns true if the toolCall is an `fs_read` call with `op=read_attach`.
/// Used by chat-interface to suppress the default image-preview rendering for
/// these calls (the badge handles the attachment display itself).
bool isFsReadAttachCall(String toolName, String argumentsJson) {
  if (!fsReadToolNames.contains(toolName)) return false;
  try {
    final args = Map<String, dynamic>.from(
      jsonDecode(argumentsJson.isEmpty ? '{}' : argumentsJson) as Map,
    );
    return (args['op'] as String?) == 'read_attach';
  } catch (_) {
    return false;
  }
}

List<Attachment> _attachmentsFromMessage(Message? msg) {
  if (msg == null) return const [];
  final out = <Attachment>[];
  for (final part in msg.content) {
    final att = _attachmentFromPart(part);
    if (att != null) out.add(att);
  }
  return out;
}

Attachment? _attachmentFromPart(ContentPart part) {
  if (part.type == 'image_url' && part.imageUrl != null) {
    return Attachment(
      type: 'image_url',
      content: part.imageUrl!.url,
      filename: part.filename,
    );
  }
  if (part.type == 'file' && (part.text ?? '').isNotEmpty) {
    return Attachment(
      type: 'file',
      content: part.text!,
      filename: part.filename,
    );
  }
  if (isDocumentType(part.type) && (part.text ?? '').isNotEmpty) {
    return Attachment(
      type: part.type,
      content: part.text!,
      filename: part.filename,
      ext: part.type,
    );
  }
  return null;
}

class _ReadAttachPreview extends StatelessWidget {
  final String path;
  final List<Attachment> attachments;

  const _ReadAttachPreview({required this.path, required this.attachments});

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      mainAxisSize: MainAxisSize.min,
      children: [
        _PathHeader(path: path),
        const SizedBox(height: 6),
        if (attachments.isEmpty)
          const _PendingPlaceholder()
        else
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              for (final att in attachments)
                AttachmentViewer(
                  attachment: att,
                  editMode: false,
                  removeAttachment: (_) {},
                ),
            ],
          ),
      ],
    );
  }
}

class _PathHeader extends StatelessWidget {
  final String path;
  const _PathHeader({required this.path});

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
          Icon(
            Icons.attach_file,
            size: 14,
            color: Theme.of(context).colorScheme.onSurfaceVariant,
          ),
          const SizedBox(width: 6),
          Text(
            'read_attach',
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

class _PendingPlaceholder extends StatelessWidget {
  const _PendingPlaceholder();

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 6),
      decoration: BoxDecoration(
        color: Theme.of(context)
            .colorScheme
            .surfaceContainerHighest
            .withOpacity(0.4),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          SizedBox(
            width: 12,
            height: 12,
            child: CircularProgressIndicator(
              strokeWidth: 2,
              color: Theme.of(context).colorScheme.onSurfaceVariant,
            ),
          ),
          const SizedBox(width: 8),
          Text(
            'reading…',
            style: TextStyle(
              fontFamily: 'monospace',
              fontSize: 12,
              color: Theme.of(context).colorScheme.onSurfaceVariant,
            ),
          ),
        ],
      ),
    );
  }
}
