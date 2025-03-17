import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import '../../api/service.dart';
import 'package:super_sliver_list/super_sliver_list.dart';
import 'folder-item.dart';
import '../../utils/types.dart';
import 'conversation-item.dart';

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

    return Column(
      children: [
        if (!kIsWeb) SizedBox(height: 20),

        // Search bar widget
        Padding(
          padding: const EdgeInsets.all(8.0),
          child: TextField(
            decoration: InputDecoration(
              hintText: 'Search conversations',
              prefixIcon: Icon(Icons.search),
              border: null,
            ),
            onChanged: (value) {
              setState(() {
                searchQuery = value;
              });
            },
          ),
        ),

        // Folders and conversations list
        Expanded(
          child: SuperListView.builder(
            itemCount: folders.length,
            itemBuilder: (context, folderIndex) {
              final folder = folders[folderIndex];
              final folderName = folder['name'] as String;
              final conversations =
                  folder['conversations'] as List<Conversation>;

              return FolderItem(
                folderName: folderName,
                isExpanded: _folderExpansionState[folderName] ?? true,
                onToggle: () => _toggleFolderExpansion(folderName),
                children:
                    conversations.map((conv) {
                      final convKey =
                          conv.id + conv.title + (conv.folder ?? '');

                      if (convCacheFuckYouFlutter.containsKey(convKey)) {
                        return convCacheFuckYouFlutter[convKey]!;
                      }

                      final convItem = ConversationItem(
                        key: ValueKey(convKey),
                        conversation: conv,
                        isSelected: widget.selectedConversationId == conv.id,
                        onSelect: widget.onConversationSelected,
                        ref: ref,
                        onTitleUpdate: widget.onTitleUpdate,
                      );

                      convCacheFuckYouFlutter[convKey] = convItem;

                      return convItem;
                    }).toList(),
              );
            },
          ),
        ),
      ],
    );
  }
}

class TestConv extends StatelessWidget {
  final Conversation conversation;

  const TestConv({Key? key, required this.conversation}) : super(key: key);

  @override
  Widget build(BuildContext context) {
    print('test');
    return Text(
      conversation.title,
      key: ValueKey(conversation.id + conversation.title),
    );
  }
}
