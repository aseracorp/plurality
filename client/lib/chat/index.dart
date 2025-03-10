import 'dart:math';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/chat/chat-interface.dart';
import '../auth/auth-service.dart';
import '../api/service.dart';
import '../api/api.dart';
import '../auth/account.dart';
import './budget.dart';

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
  final ApiService _apiService = ApiService();

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
  void initState() {
    super.initState();
    _apiService.CheckVerifyEmail(
      () => {Navigator.pushNamed(context, '/verify-email')},
    );
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
        'icon': Icon(
          Icons.home,
          color: widget.isMobile ? Theme.of(context).colorScheme.primary : null,
        ),
        'label': Text(
          'Home',
          style: TextStyle(
            color:
                widget.isMobile ? Theme.of(context).colorScheme.primary : null,
          ),
        ),
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
        'icon': Icon(
          Icons.message,
          color: widget.isMobile ? Theme.of(context).colorScheme.primary : null,
        ),
        'label': Text(
          'Conversations',
          style: TextStyle(
            color:
                widget.isMobile ? Theme.of(context).colorScheme.primary : null,
          ),
        ),
        'content': buildMessagesContent(
          context,
          conversations,
          widget.isMobile,
        ),
      },
      {
        'icon': Icon(
          Icons.account_circle,
          color: widget.isMobile ? Theme.of(context).colorScheme.primary : null,
        ),
        'label': Text(
          'Account',
          style: TextStyle(
            color:
                widget.isMobile ? Theme.of(context).colorScheme.primary : null,
          ),
        ),
        'content': SettingsScreen(),
        'hiddenOnDesktop': true,
      },
      {
        'icon': BalanceProgressCircle(isClickable: false),
        'label': Text('Budget'),
        'content': BudgetScreen(),
        'hiddenOnDesktop': true,
      },
    ];

    // For mobile: use Scaffold with bottom navigation
    if (widget.isMobile) {
      return Scaffold(
        appBar:
            _selectedConversationId != null
                ? AppBar(
                  title:
                      _selectedConversationId != null
                          ? Text(buildTitle(convTitle) ?? '')
                          : Text('Plurality Chat'),
                  leading: IconButton(
                    icon: Icon(Icons.arrow_back),
                    onPressed: () {
                      setState(() {
                        _selectedConversationId = null;
                      });
                    },
                  ),
                )
                : null,
        body: destinations[_selectedIndex]['content'] as Widget,
        bottomNavigationBar:
            _selectedConversationId == null
                ? BottomNavigationBar(
                  currentIndex: _selectedIndex,
                  selectedItemColor: Theme.of(context).colorScheme.primary,
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
                              backgroundColor:
                                  Theme.of(context).colorScheme.surface,
                              icon: dest['icon'] as Widget,
                              label: (dest['label'] as Text).data,
                            ),
                          )
                          .toList(),
                )
                : null,
      );
    }

    var desktopMaxIndex =
        destinations.where((dest) => !(dest['hiddenOnDesktop'] == true)).length;

    // For desktop: use the original layout with NavigationRail
    return Scaffold(
      body: Row(
        children: [
          NavigationRail(
            leading: Image.asset(
              'assets/logo_64.png',
              width: 48.0,
              height: 48.0,
            ),
            selectedIndex:
                desktopMaxIndex > _selectedIndex ? _selectedIndex : 0,
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
            destinations: [
              ...destinations
                  .where(
                    (dest) =>
                        !(dest['hiddenOnDesktop'] == true && !widget.isMobile),
                  )
                  .map(
                    (dest) => NavigationRailDestination(
                      icon: dest['icon'] as Widget,
                      label: dest['label'] as Widget,
                    ),
                  )
                  .toList(),
            ],
            trailing: Expanded(
              child: Align(
                alignment: Alignment.bottomCenter,
                child: Padding(
                  padding: const EdgeInsets.only(bottom: 20.0),
                  child: Column(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      BalanceProgressCircle(),
                      Divider(),
                      IconButton(
                        icon: Icon(Icons.account_circle),
                        onPressed: () async {
                          setState(() {
                            _selectedIndex = 2;
                          });
                        },
                      ),
                    ],
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
