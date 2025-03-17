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

  const ConversationList({
    Key? key,
    required this.isMobile,
    required this.selectedConversationId,
    required this.onConversationSelected,
    required this.onTitleUpdate,
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
          final convKey = conv.id + conv.title + (conv.folder ?? '');
          
          if (!convCacheFuckYouFlutter.containsKey(convKey)) {
            convCacheFuckYouFlutter[convKey] = ConversationItem(
              key: ValueKey(convKey),
              conversation: conv,
              isSelected: widget.selectedConversationId == conv.id,
              onSelect: widget.onConversationSelected,
              ref: ref,
              onTitleUpdate: widget.onTitleUpdate,
            );
          }
          
          allItems.add(convCacheFuckYouFlutter[convKey]!);
        }
      }
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (
          !kIsWeb && 
          widget.isMobile &&
          (Platform.isAndroid || Platform.isIOS)
          ) SizedBox(height: 24),

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
        Expanded(
          child: SuperListView(
            shrinkWrap: true,
            children: allItems,
          ),
        ),
      ],
    );
  }
}
