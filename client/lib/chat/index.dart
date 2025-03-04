import 'dart:math';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/chat/chat-interface.dart';
import '../auth/auth-service.dart';
import '../api/service.dart';

class ChatScreen extends ConsumerStatefulWidget {
  final bool isMobile;

  const ChatScreen({super.key, required this.isMobile});

  @override
  ConsumerState<ChatScreen> createState() => _ChatScreenState();
}

class _ChatScreenState extends ConsumerState<ChatScreen> {
  int _selectedIndex = 0;
  String? _selectedConversationId;
  String convTitle = '';
  String searchQuery = '';

  // Extract the model selection logic to a separate method
  void _updateTitle() {
    final conversationsState = ref.read(conversationsProvider);
    final matches =
        conversationsState
            .where((conv) => conv.id == _selectedConversationId)
            .toList();

    if (matches.isNotEmpty) {
      setState(() {
        convTitle = matches[0].title;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    // Check if we're on a mobile device
    // final isMobile = false;
    //MediaQuery.of(context).size.width < 600;

    // You can access your conversations provider here
    final conversations = ref.watch(conversationsProvider);

    // Navigation destinations
    var destinations = [
      {
        'icon': Icon(Icons.home),
        'label': Text('Home'),
        'content': ChatInterface(
          isMobile: widget.isMobile,
          conversationId: _selectedConversationId ?? '',
          setConversationID: (id, navigate) {
            setState(() {
              _selectedConversationId = id;
              if (navigate) _selectedIndex = 1;
            });
          },
        ),
      },
      {
        'icon': Icon(Icons.message),
        'label': Text('Conversations'),
        'content': buildMessagesContent(
          context,
          conversations,
          widget.isMobile,
        ),
      },
    ];

    // For mobile: use Scaffold with bottom navigation
    if (widget.isMobile) {
      return Scaffold(
        appBar: AppBar(
          title:
              _selectedConversationId != null
                  ? Text(buildTitle(convTitle) ?? '')
                  : Text('Plurality Chat'),
          leading:
              _selectedConversationId != null
                  ? IconButton(
                    icon: Icon(Icons.arrow_back),
                    onPressed: () {
                      setState(() {
                        _selectedConversationId = null;
                      });
                    },
                  )
                  : null,
          actions: [
            IconButton(
              icon: Icon(Icons.logout),
              onPressed: () async {
                final authService = AuthService();
                await authService.signOut();
              },
            ),
          ],
        ),
        body: destinations[_selectedIndex]['content'] as Widget,
        bottomNavigationBar:
            _selectedConversationId == null
                ? BottomNavigationBar(
                  currentIndex: _selectedIndex,
                  onTap: (index) {
                    setState(() {
                      if (index != _selectedIndex) {
                        _selectedConversationId = null;
                      }
                      _selectedIndex = index;
                    });
                  },
                  items:
                      destinations
                          .map(
                            (dest) => BottomNavigationBarItem(
                              icon: dest['icon'] as Widget,
                              label: (dest['label'] as Text).data,
                            ),
                          )
                          .toList(),
                )
                : null,
      );
    }

    // For desktop: use the original layout with NavigationRail
    return Scaffold(
      body: Row(
        children: [
          NavigationRail(
            selectedIndex: _selectedIndex,
            onDestinationSelected: (int index) {
              setState(() {
                if (index == 0) {
                  _selectedConversationId = null;
                }
                _selectedIndex = index;

                if (index == 1 &&
                    !widget.isMobile &&
                    conversations.isNotEmpty &&
                    _selectedConversationId == null) {
                  // select first
                  _selectedConversationId = conversations[0].id;
                }
              });
            },
            destinations:
                destinations
                    .map(
                      (dest) => NavigationRailDestination(
                        icon: dest['icon'] as Widget,
                        label: dest['label'] as Widget,
                      ),
                    )
                    .toList(),
            trailing: Expanded(
              child: Align(
                alignment: Alignment.bottomCenter,
                child: Padding(
                  padding: const EdgeInsets.only(bottom: 20.0),
                  child: IconButton(
                    icon: Icon(Icons.logout),
                    onPressed: () async {
                      final authService = AuthService();
                      await authService.signOut();
                    },
                  ),
                ),
              ),
            ),
          ),
          // Vertical divider
          VerticalDivider(thickness: 1, width: 1),

          // Main content area
          Expanded(child: destinations[_selectedIndex]['content'] as Widget),
        ],
      ),
    );
  }

  Widget buildConversationsList(List<dynamic> conversations, bool isMobile) {
    return conversations.isEmpty
        ? Center(
          child: Text(
            'No conversations yet',
            style: Theme.of(context).textTheme.titleMedium,
          ),
        )
        : Column(
          children: [
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
            // ListView needs to be in an Expanded widget when inside a Column
            Expanded(
              child: ListView.builder(
                itemCount: conversations.length,
                itemBuilder: (context, index) {
                  final conv = conversations[index];
                  if (searchQuery.length < 3 ||
                      conv.title.toLowerCase().contains(
                        searchQuery.toLowerCase(),
                      )) {
                    return ListTile(
                      key: ValueKey(conv.id),
                      title: Text(
                        buildTitle(conv.title),
                        maxLines: 2,
                        style: TextStyle(
                          fontWeight: FontWeight.bold,
                          overflow: TextOverflow.ellipsis,
                          fontSize: 14,
                        ),
                      ),
                      subtitle: Text(
                        conv.lastMessageAt.toString().substring(0, 16),
                        style: TextStyle(fontSize: 11),
                      ),
                      trailing: IconButton(
                        icon: const Icon(Icons.delete),
                        onPressed: () {
                          ref
                              .read(conversationsProvider.notifier)
                              .deleteConversation(conv.id);
                          if (_selectedConversationId == conv.id) {
                            setState(() {
                              _selectedConversationId = null;
                            });
                          }
                        },
                      ),
                      selected: _selectedConversationId == conv.id,
                      onTap: () {
                        setState(() {
                          _selectedConversationId = conv.id;
                        });
                        _updateTitle();
                      },
                    );
                  } else {
                    return Container();
                  }
                },
              ),
            ),
          ],
        );
  }

  // Widget to build the messages content (differs between mobile and desktop)
  Widget buildMessagesContent(
    BuildContext context,
    List<dynamic> conversations,
    bool isMobile,
  ) {
    if (isMobile) {
      // On mobile, just show the chat interface
      if (_selectedConversationId == null) {
        return Center(child: buildConversationsList(conversations, isMobile));
      } else {
        return ChatInterface(
          conversationId: _selectedConversationId ?? '',
          isMobile: isMobile,
          setConversationID: (id, navigate) {
            setState(() {
              _selectedConversationId = id;
              if (navigate) _selectedIndex = 1;
            });
          },
        );
      }
    } else {
      // On desktop, show the row with conversations list and chat
      return Row(
        children: [
          // Conversations list (second rail)
          Container(
            width: 250,
            child: buildConversationsList(conversations, isMobile),
          ),
          // Vertical divider between conversation list and chat
          VerticalDivider(thickness: 1, width: 1),
          // Chat interface
          Expanded(
            child: ChatInterface(
              isMobile: isMobile,
              conversationId: _selectedConversationId ?? '',
              setConversationID: (id, navigate) {
                setState(() {
                  _selectedConversationId = id;
                  if (navigate) _selectedIndex = 1;
                });
              },
            ),
          ),
        ],
      );
    }
  }
}

String buildTitle(String title) {
  title = title == "" ? 'Untitled' : title.replaceAll(RegExp(r'\*\*'), '');
  title = (title.length > 100 ? '${title.substring(0, 100)}' : title);
  return title;
}
