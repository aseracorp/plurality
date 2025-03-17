import 'dart:math';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/chat/message-list/chat-interface.dart';
import '../../auth/auth-service.dart';
import '../../api/service.dart';
import '../../api/tts.dart';
import '../../utils/types.dart';
import '../../auth/account.dart';
import '../budget.dart';
import 'conversation-list.dart';

class ConversationItem extends StatelessWidget {
  final Conversation conversation;
  final bool isSelected;
  final Function(String) onSelect;
  final WidgetRef ref;
  final Function() onTitleUpdate;

  const ConversationItem({
    Key? key,
    required this.conversation,
    required this.isSelected,
    required this.onSelect,
    required this.ref,
    required this.onTitleUpdate,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return ListTile(
      key: ValueKey(conversation.id),
      title: Text(
        conversation.title,
        maxLines: 2,
        style: TextStyle(
          fontWeight: FontWeight.bold,
          overflow: TextOverflow.ellipsis,
          fontSize: 14,
        ),
      ),
      subtitle: Text(
        conversation.lastMessageAt.toString().substring(0, 16),
        style: TextStyle(fontSize: 11),
      ),
      trailing: PopupMenuButton<String>(
        icon: Icon(Icons.more_vert),
        onSelected: (value) async {
          if (value == 'rename') {
            await _handleRename(context);
          } else if (value == 'move') {
            await _handleMove(context);
          } else if (value == 'delete') {
            _handleDelete();
          } else if (value == 'pin') {
            ref
                .read(conversationsProvider.notifier)
                .updateConversationFolder(conversation.id, 'Pinned');
          } else if (value == 'unpin') {
            ref
                .read(conversationsProvider.notifier)
                .updateConversationFolder(conversation.id, '');
          }
        },
        itemBuilder:
            (context) => [
              PopupMenuItem<String>(
                value: 'rename',
                child: Row(
                  children: [
                    Icon(Icons.edit, size: 20),
                    SizedBox(width: 8),
                    Text('Rename'),
                  ],
                ),
              ),
              PopupMenuItem<String>(
                value: conversation.folder == 'Pinned' ? 'unpin' : 'pin',
                child: Row(
                  children: [
                    conversation.folder == 'Pinned'
                        ? Icon(Icons.push_pin, size: 20)
                        : Icon(Icons.push_pin_outlined, size: 20),
                    SizedBox(width: 8),
                    conversation.folder == 'Pinned'
                        ? Text('Unpin')
                        : Text('Pin'),
                  ],
                ),
              ),
              PopupMenuItem<String>(
                value: 'move',
                child: Row(
                  children: [
                    Icon(Icons.folder, size: 20),
                    SizedBox(width: 8),
                    Text('Move to Folder'),
                  ],
                ),
              ),
              PopupMenuItem<String>(
                value: 'delete',
                child: Row(
                  children: [
                    Icon(Icons.delete, size: 20, color: Colors.red),
                    SizedBox(width: 8),
                    Text('Delete', style: TextStyle(color: Colors.red)),
                  ],
                ),
              ),
            ],
      ),
      selected: isSelected,
      onTap: () => onSelect(conversation.id),
    );
  }

  Future<void> _handleRename(BuildContext context) async {
    final TextEditingController titleController = TextEditingController(
      text: conversation.title,
    );

    final newTitle = await showDialog<String>(
      context: context,
      builder:
          (context) => AlertDialog(
            title: Text('Rename Conversation'),
            content: TextField(
              controller: titleController,
              decoration: InputDecoration(hintText: 'Enter new title'),
              autofocus: true,
            ),
            actions: [
              TextButton(
                onPressed: () => Navigator.pop(context),
                child: Text('Cancel'),
              ),
              TextButton(
                onPressed: () => Navigator.pop(context, titleController.text),
                child: Text('Save'),
              ),
            ],
          ),
    );

    if (newTitle != null && newTitle.isNotEmpty) {
      try {
        ref
            .read(conversationsProvider.notifier)
            .updateConversationTitle(conversation.id, newTitle);

        onTitleUpdate();

        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('Conversation renamed')));
      } catch (e) {
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('Failed to rename: $e')));
      }
    }
  }

  Future<void> _handleMove(BuildContext context) async {
    final conversationsState = ref.read(conversationsProvider);
    Map<String, List<dynamic>> folderMap = {};

    // TODO Use folder list instead
    for (var conv in conversationsState.conversations) {
      String folderName = conv.folder ?? "";
      if (folderName != "" && folderName != "Pinned") {
        if (!folderMap.containsKey(folderName)) {
          folderMap[folderName] = [];
        }
        folderMap[folderName]!.add(conv);
      }
    }

    final folderNames = folderMap.keys.toList();
    folderNames.add("+ Create New Folder");

    final selectedFolder = await showDialog<String>(
      context: context,
      builder:
          (context) => AlertDialog(
            title: Text('Move to Folder'),
            content: Container(
              width: 500,
              child: ListView.builder(
                shrinkWrap: true,
                itemCount: folderNames.length,
                itemBuilder: (context, index) {
                  return ListTile(
                    leading:
                        index < folderNames.length - 1
                            ? Icon(Icons.folder)
                            : Icon(Icons.create_new_folder),
                    title: Text(folderNames[index]),
                    onTap: () => Navigator.pop(context, folderNames[index]),
                  );
                },
              ),
            ),
            actions: [
              TextButton(
                onPressed: () => Navigator.pop(context, ""),
                child: Text('Remove From Folder'),
              ),
              TextButton(
                onPressed: () => Navigator.pop(context),
                child: Text('Cancel'),
              ),
            ],
          ),
    );

    if (selectedFolder != null) {
      if (selectedFolder == "+ Create New Folder") {
        await _createAndMoveToNewFolder(context);
      } else {
        try {
          ref
              .read(conversationsProvider.notifier)
              .updateConversationFolder(conversation.id, selectedFolder);

          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(content: Text('Moved to folder: $selectedFolder')),
          );
        } catch (e) {
          ScaffoldMessenger.of(
            context,
          ).showSnackBar(SnackBar(content: Text('Failed to move: $e')));
        }
      }
    }
  }

  Future<void> _createAndMoveToNewFolder(BuildContext context) async {
    final TextEditingController folderController = TextEditingController();

    final newFolderName = await showDialog<String>(
      context: context,
      builder:
          (context) => AlertDialog(
            title: Text('Create New Folder'),
            content: TextField(
              controller: folderController,
              decoration: InputDecoration(hintText: 'Enter folder name'),
              autofocus: true,
            ),
            actions: [
              TextButton(
                onPressed: () => Navigator.pop(context),
                child: Text('Cancel'),
              ),
              TextButton(
                onPressed: () => Navigator.pop(context, folderController.text),
                child: Text('Create'),
              ),
            ],
          ),
    );

    if (newFolderName != null && newFolderName.isNotEmpty) {
      try {
        ref
            .read(conversationsProvider.notifier)
            .updateConversationFolder(conversation.id, newFolderName);

        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text('Moved to new folder: $newFolderName')),
        );
      } catch (e) {
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('Failed to move: $e')));
      }
    }
  }

  void _handleDelete() {
    ref
        .read(conversationsProvider.notifier)
        .deleteConversation(conversation.id);
  }
}
