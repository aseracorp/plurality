import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/chat/conversation-list/conversation-item.dart';
import 'package:plurality/chat/message-list/chat-interface.dart';
import 'package:plurality/utils/index.dart';
import 'package:plurality/utils/deep_link.dart';
import '../api/service.dart';
import '../api/api.dart';
import '../utils/types.dart';
import '../auth/account.dart';
import '../cron/cron-list.dart';
import '../webhook/webhook-list.dart';
import '../preset/preset-list.dart';
import 'conversation-list/conversation-list.dart';
import 'dart:convert';
import 'dart:math';
import 'dart:typed_data';
import '../utils/image_loader.dart';

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
  final ApiService _apiService = ApiService();
  Conversation? selectedConv;
  bool hasUpdate = false;

  // Extract the model selection logic to a separate method
  void _updateTitle() {
    final conversationsState = ref.read(conversationsProvider);
    final matches =
        conversationsState.conversations
            .where((conv) => conv.id == _selectedConversationId)
            .toList();

    if (matches.isNotEmpty) {
      setState(() {
        selectedConv = matches[0];
        convTitle = matches[0].title;
      });
    }
  }

  @override
  void initState() {
    super.initState();
    final pending = ref.read(pendingConversationIdProvider);
    if (pending != null && pending.isNotEmpty) {
      _selectedConversationId = pending;
      _selectedIndex = 1;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        ref.read(pendingConversationIdProvider.notifier).state = null;
        _updateTitle();
      });
    }
    checkVersion().then((value) {
      if (value) {
        showDialog(
          context: context,
          builder: (context) {
            return AlertDialog(
              title: Text('Update Available'),
              content: Text(
                'A new version of the app is available. Please update to the latest version.',
              ),
              actions: [
                TextButton(
                  onPressed: () {
                    Navigator.of(context).pop();
                  },
                  child: Text('OK'),
                ),
              ],
            );
          },
        );
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    final conversations = ref.watch(conversationsProvider).conversations;

    // Update title reactively when provider changes (e.g. async title generation)
    ref.listen(conversationsProvider, (previous, next) {
      if (_selectedConversationId != null) {
        _updateTitle();
      }
    });

    // Open a conversation when a plurality:// deep link arrives while the app is running.
    ref.listen<String?>(pendingConversationIdProvider, (previous, next) {
      if (next == null || next.isEmpty) return;
      setState(() {
        _selectedConversationId = next;
        _selectedIndex = 1;
        _updateTitle();
      });
      ref.read(pendingConversationIdProvider.notifier).state = null;
    });

    // Navigation destinations
    var destinations = [
      {
        'icon': Icon(
          Icons.home,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Home',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
          ),
        ),
        'content': ChatInterface(
          isMobile: widget.isMobile,
          updateMainTitle: _updateTitle,
          conversationId: _selectedConversationId ?? '',
          setConversationID: (id, navigate) {
            setState(() {
              _selectedConversationId = id;
              if (navigate) _selectedIndex = 1;

              _updateTitle();
            });
          },
        ),
      },
      {
        'icon': Icon(
          Icons.message,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Conversations',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
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
          Icons.dashboard_customize,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Presets',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
          ),
        ),
        'content': const PresetListScreen(),
      },
      {
        'icon': Icon(
          Icons.schedule,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Schedules',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
          ),
        ),
        'content': const CronListScreen(),
      },
      {
        'icon': Icon(
          Icons.webhook,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Webhooks',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
          ),
        ),
        'content': const WebhookListScreen(),
      },
      {
        'icon': Icon(
          Icons.account_circle,
          color:
              widget.isMobile
                  ? Theme.of(context).colorScheme.primary
                  : Colors.white,
        ),
        'label': Text(
          'Account',
          style: TextStyle(
            color:
                widget.isMobile
                    ? Theme.of(context).colorScheme.primary
                    : Colors.white,
          ),
        ),
        'content': SettingsScreen(),
        'hiddenOnDesktop': true,
      },
    ];

    if (widget.isMobile) {
      return PopScope(
        canPop: _selectedIndex != 1,
        onPopInvoked: (didPop) {
          if (_selectedConversationId != null) {
            setState(() {
              _selectedConversationId = null;
            });
          } else {
            setState(() {
              _selectedIndex = 0;
            });
          }
        },
        child: Scaffold(
          appBar:
              _selectedConversationId != null
                  ? AppBar(
                    title:
                        _selectedConversationId != null
                            ? Row(
                              children: [
                                if (selectedConv != null)
                                  CircleAvatar(
                                    backgroundColor:
                                        selectedConv!.icon != null &&
                                                selectedConv!.icon!.isNotEmpty
                                            ? Colors.transparent
                                            : Colors.primaries[Random().nextInt(
                                              Colors.primaries.length,
                                            )],
                                    child:
                                        selectedConv!.icon != null &&
                                                selectedConv!.icon!.isNotEmpty
                                            ? ClipOval(
                                              child: FutureBuilder<Uint8List>(
                                                future: loadImageBytes(selectedConv!.icon!),
                                                builder: (context, snapshot) {
                                                  if (snapshot.hasData) {
                                                    return Image.memory(
                                                      snapshot.data!,
                                                      width: 250,
                                                      height: 250,
                                                      fit: BoxFit.cover,
                                                    );
                                                  }
                                                  return SizedBox(width: 40, height: 40);
                                                },
                                              ),
                                            )
                                            : Text(
                                              convTitle.isNotEmpty
                                                  ? convTitle[0].toUpperCase()
                                                  : '?',
                                              style: TextStyle(
                                                color: Colors.white,
                                              ),
                                            ),
                                  ),
                                if (selectedConv != null) SizedBox(width: 24),
                                Text(buildTitle(convTitle) ?? ''),
                              ],
                            )
                            : Text('Plurality Chat'),
                    leading: IconButton(
                      icon: Icon(Icons.arrow_back),
                      onPressed: () {
                        setState(() {
                          _selectedConversationId = null;
                        });
                      },
                    ),
                    actions: [
                      if (selectedConv != null)
                        ConversationItem(
                          conversation: selectedConv!,
                          isSelected: false,
                          onSelect: (t) {},
                          ref: ref,
                          menuOnly: true,
                          onTitleUpdate: _updateTitle,
                          onDelete: () {
                            setState(() {
                              _selectedConversationId = null;
                            });
                          },
                        ),
                    ],
                  )
                  : null,
          body: SafeArea(
            top: _selectedConversationId == null,
            bottom: _selectedConversationId != null,
            child: destinations[_selectedIndex]['content'] as Widget,
          ),
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
        ),
      );
    }

    var desktopMaxIndex =
        destinations.where((dest) => !(dest['hiddenOnDesktop'] == true)).length;

    // For desktop: use the original layout with NavigationRail
    return Scaffold(
      body: Row(
        children: [
          NavigationRail(
            backgroundColor: Theme.of(context).primaryColor,
            // leading: Image.asset(
            //   'assets/logo_64.png',
            //   width: 48.0,
            //   height: 48.0,
            // ),
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
                      IconButton(
                        icon: Icon(Icons.account_circle, color: Colors.white),
                        onPressed: () async {
                          setState(() {
                            _selectedIndex = destinations.indexWhere(
                              (dest) => dest['content'] is SettingsScreen,
                            );
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

  // Widget to build the messages content (differs between mobile and desktop)
  Widget buildMessagesContent(
    BuildContext context,
    List<dynamic> conversations,
    bool isMobile,
  ) {
    if (isMobile) {
      // On mobile, just show the chat interface
      if (_selectedConversationId == null) {
        return Center(
          child: ConversationList(
            isMobile: isMobile,
            selectedConversationId: _selectedConversationId,
            onDelete: () {
              setState(() {
                _selectedConversationId = null;
              });
            },
            onConversationSelected: (id) {
              setState(() {
                _selectedConversationId = id;
              });
              _updateTitle();
            },
            onTitleUpdate: _updateTitle,
          ),
        );
      } else {
        return ChatInterface(
          conversationId: _selectedConversationId ?? '',
          isMobile: isMobile,
          updateMainTitle: _updateTitle,
          setConversationID: (id, navigate) {
            setState(() {
              _selectedConversationId = id;
              if (navigate) _selectedIndex = 1;
              _updateTitle();
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
            width: 265,
            child: ConversationList(
              isMobile: isMobile,
              onDelete: () {
                setState(() {
                  _selectedConversationId = null;
                });
              },
              selectedConversationId: _selectedConversationId,
              onConversationSelected: (id) {
                setState(() {
                  _selectedConversationId = id;
                });
                _updateTitle();
              },
              onTitleUpdate: _updateTitle,
            ),
          ),
          // Vertical divider between conversation list and chat
          VerticalDivider(thickness: 1, width: 1),
          // Chat interface
          Expanded(
            child: ChatInterface(
              isMobile: isMobile,
              conversationId: _selectedConversationId ?? '',
              updateMainTitle: _updateTitle,
              setConversationID: (id, navigate) {
                setState(() {
                  _selectedConversationId = id;
                  if (navigate) _selectedIndex = 1;
                  _updateTitle();
                });
              },
            ),
          ),
        ],
      );
    }
  }
}

// Keep this helper function
String buildTitle(String title) {
  title = title == "" ? 'Untitled' : title.replaceAll(RegExp(r'\*\*'), '');
  title = (title.length > 100 ? '${title.substring(0, 100)}' : title);
  return title;
}
