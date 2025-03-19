import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import '../../api/service.dart';
import 'package:super_sliver_list/super_sliver_list.dart';
import '../../utils/types.dart';
import 'conversation-item.dart';
import 'folder-item.dart';
import 'dart:io' show Platform;

class ConversationList extends ConsumerStatefulWidget {
  final bool isMobile;
  final String? selectedConversationId;
  final Function(String) onConversationSelected;
  final Function() onTitleUpdate;
  final Function() onDelete;

  const ConversationList({
    Key? key,
    required this.isMobile,
    required this.selectedConversationId,
    required this.onConversationSelected,
    required this.onTitleUpdate,
    required this.onDelete,
  }) : super(key: key);

  @override
  ConsumerState<ConversationList> createState() => _ConversationListState();
}

class _ConversationListState extends ConsumerState<ConversationList> {
  String searchQuery = '';
  Map<String, bool> _folderExpansionState = {};
  Map<String, Widget> convCacheFuckYouFlutter = {};

  // Toggle folder expansion state
  void _toggleFolderExpansion(String folderName) {
    setState(() {
      _folderExpansionState[folderName] =
          !(_folderExpansionState[folderName] ?? true);
    });
  }

  @override
  Widget build(BuildContext context) {
    // Use the sorted folders provider with search query
    final folders = ref.watch(
      sortedFoldersProvider(searchQuery.length >= 3 ? searchQuery : null),
    );

    // Initialize folder expansion state for any new folders
    for (var folder in folders) {
      _folderExpansionState.putIfAbsent(folder['name'] as String, () => true);
    }

    if (folders.isEmpty) {
      return Center(
        child: Text(
          'No conversations yet',
          style: Theme.of(context).textTheme.titleMedium,
        ),
      );
    }

    // Create a flat list of all items (folders and conversations)
    List<Widget> allItems = [];

    for (var folder in folders) {
      final folderName = folder['name'] as String;
      final conversations = folder['conversations'] as List<Conversation>;
      final isExpanded = _folderExpansionState[folderName] ?? true;

      // Add folder header
      allItems.add(
        FolderItem(
          key: ValueKey(folderName),
          folderName: folderName,
          isExpanded: isExpanded,
          // itemCount: conversations.length,
          onToggle: _toggleFolderExpansion,
        ),
      );

      // Add conversations if folder is expanded
      if (isExpanded) {
        for (var conv in conversations) {
          final selected = (widget.selectedConversationId == conv.id);
          final convKey =
              conv.id +
              conv.title +
              (conv.folder ?? '') +
              (selected ? 'Y' : '');

          if (!convCacheFuckYouFlutter.containsKey(convKey)) {
            convCacheFuckYouFlutter[convKey] = ConversationItem(
              key: ValueKey(convKey),
              conversation: conv,
              isSelected: selected,
              onSelect: widget.onConversationSelected,
              ref: ref,
              onTitleUpdate: widget.onTitleUpdate,
              onDelete: widget.onDelete,
            );
          }

          allItems.add(convCacheFuckYouFlutter[convKey]!);
        }
      }
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (!kIsWeb &&
            widget.isMobile &&
            (Platform.isAndroid || Platform.isIOS))
          SizedBox(height: 20),
        SizedBox(height: 8),

        // new conversation button
        if (!widget.isMobile)
          Padding(
            padding: const EdgeInsets.all(8.0),
            child: ElevatedButton(
              onPressed: () {
                widget.onDelete();
              },
              child: Text('New conversation'),
            ),
          ),

        // Search bar widget
        Padding(
          padding: const EdgeInsets.all(8.0),
          child: TextField(
            decoration: InputDecoration(
              hintText: 'Search conversations',
              prefixIcon: Icon(Icons.search),
            ),
            onChanged: (value) {
              setState(() {
                searchQuery = value;
              });
            },
          ),
        ),

        // Single flat list for folders and conversations
        Expanded(child: SuperListView(shrinkWrap: true, children: allItems)),
      ],
    );
  }
}
