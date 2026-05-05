import 'dart:math';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/chat/message-list/chat-interface.dart';
import '../../auth/auth-service.dart';
import '../../api/service.dart';
import '../../api/api.dart';
import '../../api/tts.dart';
import '../../auth/account.dart';
import 'conversation-list.dart';

class FolderItem extends StatelessWidget {
  final String folderName;
  final bool isExpanded;
  final Function(String) onToggle;

  const FolderItem({
    Key? key,
    required this.folderName,
    required this.isExpanded,
    required this.onToggle,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (folderName != 'Pinned')
        Divider(),
        // Folder header
        if (folderName != '')
          InkWell(
            onTap: () => onToggle(folderName),
            child: Padding(
              padding: const EdgeInsets.all(8.0),
              child: Row(
                children: [
                  Icon(
                    folderName == 'Pinned'
                        ? Icons.push_pin
                        : (isExpanded ? Icons.folder_open : Icons.folder),
                    color: Theme.of(context).colorScheme.primary,
                  ),
                  SizedBox(width: 8),
                  Expanded(
                    child: Text(
                      folderName,
                      style: TextStyle(fontWeight: FontWeight.bold),
                    ),
                  ),
                  if (folderName != 'Pinned')
                    Icon(
                      isExpanded
                          ? Icons.keyboard_arrow_up
                          : Icons.keyboard_arrow_down,
                    ),
                ],
              ),
            ),
          ),
      ],
    );
  }
}
