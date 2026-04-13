import 'dart:typed_data';
import 'package:flutter/material.dart';
import 'package:file_saver/file_saver.dart';
import 'image.dart';
import 'text-preview.dart';
import '../../utils/types.dart';
import '../../utils/file-types.dart';
import '../../api/api.dart';
import 'dart:convert';

class _DocStyle {
  final IconData icon;
  final Color iconColor;
  final Color bgColor;
  final Color borderColor;
  const _DocStyle(this.icon, this.iconColor, this.bgColor, this.borderColor);
}

_DocStyle _documentStyle(String type) {
  switch (type) {
    case 'pdf':
      return _DocStyle(Icons.picture_as_pdf, Colors.red.shade400, Colors.red.shade50, Colors.red.shade200);
    case 'docx':
      return _DocStyle(Icons.description, Colors.blue.shade400, Colors.blue.shade50, Colors.blue.shade200);
    case 'xlsx':
      return _DocStyle(Icons.table_chart, Colors.green.shade400, Colors.green.shade50, Colors.green.shade200);
    case 'pptx':
      return _DocStyle(Icons.slideshow, Colors.orange.shade400, Colors.orange.shade50, Colors.orange.shade200);
    default:
      return _DocStyle(Icons.insert_drive_file, Colors.grey.shade400, Colors.grey.shade50, Colors.grey.shade200);
  }
}

class AttachmentViewer extends StatelessWidget {
  final Attachment attachment;
  final void Function(Attachment attachment) removeAttachment;
  final bool editMode;
  final bool mini;
  final ToolCall? toolCall;
  final bool loading;

  const AttachmentViewer({
    Key? key,
    required this.attachment,
    required this.removeAttachment,
    required this.editMode,
    this.toolCall,
    this.mini = false,
    this.loading = false,
  }) : super(key: key);

  String summarizeText(int nb, String text) {
    String res = "";
    List<String> lines = text.split('\n');
    if (lines.length > nb) {
      res = lines.sublist(0, nb).join('\n');
    } else {
      res = text;
    }

    List<String> resLines = res.split('\n');
    res = "";
    for (var line in resLines) {
      if (line.length > 100) {
        res += line.substring(0, 100) + '...\n';
      } else {
        res += line + '\n';
      }
    }

    return res;
  }

  Future<void> _downloadFile(BuildContext context) async {
    try {
      final content = attachment.content;
      final filename = attachment.filename ?? 'document.${attachment.ext ?? attachment.type}';
      Uint8List bytes;

      if (content.startsWith('/attachments/')) {
        // Internal URL — fetch from server
        bytes = await ApiService().fetchAttachmentBytes(content);
      } else if (content.startsWith('data:')) {
        // Data URI — decode base64
        bytes = base64Decode(content.split(',').last);
      } else {
        return;
      }

      await FileSaver.instance.saveFile(
        name: filename,
        bytes: bytes,
      );
    } catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text('Failed to download: $e'), showCloseIcon: true),
        );
      }
    }
  }

  Widget _buildDocumentCard(BuildContext context, {required IconData icon, required Color iconColor, required Color bgColor, required Color borderColor}) {
    final filename = attachment.filename ?? 'document.${attachment.ext ?? 'bin'}';
    return Stack(
      children: [
        Padding(
          padding: const EdgeInsets.only(right: 8.0),
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTap: editMode ? null : () => _downloadFile(context),
            child: Container(
              width: mini ? 10 : 100,
              height: mini ? 10 : 100,
              decoration: BoxDecoration(
                color: bgColor,
                borderRadius: BorderRadius.circular(8.0),
                border: Border.all(color: borderColor),
              ),
              padding: const EdgeInsets.all(8.0),
              child: Column(
                mainAxisAlignment: MainAxisAlignment.center,
                children: [
                  Icon(icon, color: iconColor, size: mini ? 8 : 32),
                  if (!mini) const SizedBox(height: 4),
                  if (!mini)
                    Text(
                      filename,
                      style: TextStyle(color: Colors.grey[800], fontSize: 9.0),
                      overflow: TextOverflow.ellipsis,
                      maxLines: 2,
                      textAlign: TextAlign.center,
                    ),
                  if (!mini && !editMode) ...[
                    const SizedBox(height: 2),
                    Icon(Icons.download, color: Colors.grey[500], size: 14),
                  ],
                ],
              ),
            ),
          ),
        ),
        if (editMode)
          Positioned(
            right: 0,
            top: 0,
            child: IconButton(
              onPressed: () {
                removeAttachment(attachment);
              },
              icon: Icon(Icons.close, color: Colors.grey),
            ),
          ),
      ],
    );
  }

  @override
  Widget build(BuildContext context) {
    if (attachment.type == "snippet" && editMode) {
      return Stack(
        children: [
          Padding(
            padding: const EdgeInsets.only(right: 8.0),
            child: Container(
              width: mini ? 10 : 100,
              height: mini ? 10 : 100,
              decoration: BoxDecoration(
                color: Colors.grey[200],
                borderRadius: BorderRadius.circular(8.0),
              ),
              padding: const EdgeInsets.all(8.0),
              child: Text(
                softWrap: false,
                summarizeText(6, attachment.content),
                style: TextStyle(color: Colors.grey[800], fontSize: 8.0),
              ),
            ),
          ),
          if (editMode)
            Positioned(
              right: 0,
              top: 0,
              child: IconButton(
                onPressed: () {
                  removeAttachment(attachment);
                },
                icon: Icon(Icons.close, color: Colors.grey),
              ),
            ),
        ],
      );
    } else if (attachment.type == "snippet") {
      return TextPreviewComponent(
        mini: mini,
        content: attachment.content,
        summary: summarizeText(10, attachment.content),
      );
    } else if (attachment.type == "image_url" && editMode) {
      // Edit mode: content is a local data: URI from the file picker
      return Stack(
        children: [
          Padding(
            padding: const EdgeInsets.only(right: 8.0),
            child: ClipRRect(
              borderRadius: BorderRadius.circular(8.0),
              child: Container(
                constraints: BoxConstraints(maxHeight: 100),
                child: Image.memory(
                  base64Decode(attachment.content.split(",").last),
                  width: mini ? 10 : 100,
                  fit: BoxFit.cover,
                  cacheWidth: 100,
                  gaplessPlayback: true,
                ),
              ),
            ),
          ),
          Positioned(
            right: 0,
            top: 0,
            child: IconButton(
              onPressed: () {
                removeAttachment(attachment);
              },
              icon: Icon(Icons.close, color: Colors.grey),
            ),
          ),
        ],
      );
    } else if (attachment.type == "image_url") {
      return ImagePreviewComponent(imageUrl: attachment.content, mini: mini);
    } else if (isDocumentType(attachment.type) || attachment.type == "file") {
      final docStyle = _documentStyle(attachment.type);
      return _buildDocumentCard(
        context,
        icon: docStyle.icon,
        iconColor: docStyle.iconColor,
        bgColor: docStyle.bgColor,
        borderColor: docStyle.borderColor,
      );
    }
    return Container();
  }
}
