import 'package:flutter/material.dart';
import './image.dart';
import './text-preview.dart';
import '../utils/types.dart';
import 'dart:convert';

class AttachmentViewer extends StatelessWidget {
  final Attachment attachment;
  final void Function(Attachment attachment) removeAttachment;
  final bool editMode;
  final bool mini;

  const AttachmentViewer({
    Key? key,
    required this.attachment,
    required this.removeAttachment,
    required this.editMode,
    this.mini = false,
  }) : super(key: key);

  String summarizeText(int nb, String text) {
    // Step 1 keep 5 lines

    String res = "";

    List<String> lines = text.split('\n');
    if (lines.length > nb) {
      res = lines.sublist(0, nb).join('\n');
    } else {
      res = text;
    }

    // Step 2 keep 15 characters per line
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
      return Stack(
        children: [
          Padding(
            padding: const EdgeInsets.only(right: 8.0),
            child: ClipRRect(
              borderRadius: BorderRadius.circular(8.0),
              child: Container(
                constraints: BoxConstraints(maxHeight: 100),
                child: Image.memory(
                  base64Decode((attachment?.content.split(",")?.last) ?? ''),
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
    }
    return Container();
  }
}
