import 'dart:async';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import './storage.dart';
import './api.dart';
import '../utils/types.dart';
import '../auth/auth-service.dart';

class ConversationState {
  final List<Conversation> conversations;
  final Map<String, List<Conversation>> folderMap;

  ConversationState({required this.conversations, required this.folderMap});
}

class FolderData {
  final String name;
  final List<Conversation> conversations;

  FolderData({required this.name, required this.conversations});
}

// In service.dart, modify the ConversationsNotifier class
class ConversationsNotifier extends StateNotifier<ConversationState> {
  final AuthService _authService = AuthService();
  StreamSubscription? _authSubscription;
  final apiService = ApiService();

  static bool _isInitialized = false;

  ConversationsNotifier()
    : super(ConversationState(conversations: [], folderMap: {})) {
    // Load initial local data
    _loadConversations();

    // Subscribe to auth changes
    _authSubscription = _authService.authStateChanges.listen((user) {
      if (user != null && !_isInitialized) {
        _isInitialized = true;
        print('User logged in, loading conversations');
        Future.microtask(() {
          refresh();
        });
      }
    });
  }

  // Refresh data
  Future<void> refresh() async {
    await _loadConversationsFromServer();
    return Future.value();
  }

  // Helper to organize conversations by folder
  Map<String, List<Conversation>> _organizeFolders(
    List<Conversation> conversations,
  ) {
    Map<String, List<Conversation>> folderMap = {};

    for (var conv in conversations) {
      String folderName = conv.folder ?? "";

      if (!folderMap.containsKey(folderName)) {
        folderMap[folderName] = [];
      }

      folderMap[folderName]!.add(conv);
    }

    return folderMap;
  }

  void _loadConversations() async {
    final conversations = await ConversationStorage.getAllConversations();
    state = ConversationState(
      conversations: conversations,
      folderMap: _organizeFolders(conversations),
    );
  }

  // Simply update all methods to maintain both state properties

  Future<void> _loadConversationsFromServer() async {
    _loadConversations();
    var newConv = await apiService.getConversations();
    await ConversationStorage.mergeConversations(newConv);
    _loadConversations();
  }

  void loadConversation(String id) async {
    if (id == '') {
      return;
    }

    var conv = await apiService.getConversation(id);
    if (conv != null) {
      await ConversationStorage.saveConversation(conv);
      _loadConversations();
    }
  }

  // Add a message
  Future<void> addMessage({
    required String conversationId,
    required Message message,
  }) async {
    final updated = await ConversationStorage.addMessage(
      conversationId: conversationId,
      message: message,
    );

    if (updated != null) {
      final updatedConversations =
          state.conversations.map((conv) {
            if (conv.id == conversationId) {
              return updated;
            }
            return conv;
          }).toList();

      state = ConversationState(
        conversations: updatedConversations,
        folderMap: _organizeFolders(updatedConversations),
      );
    }
  }

  // Update title
  Future<void> updateConversationMetaData({
    required String conversationId,
    String? title,
    ModelSelected? modelSelected,
  }) async {
    final updated = await ConversationStorage.updateConversationMetaData(
      conversationId: conversationId,
      title: title,
      modelSelected: modelSelected,
    );

    if (updated != null) {
      final updatedConversations =
          state.conversations.map((conv) {
            if (conv.id == conversationId) {
              return updated;
            }
            return conv;
          }).toList();

      state = ConversationState(
        conversations: updatedConversations,
        folderMap: _organizeFolders(updatedConversations),
      );
    }
  }

  Future<void> updateConversationTitle(String id, String title) async {
    final updated = await ConversationStorage.updateConversationTitle(
      id,
      title,
    );

    if (updated != null) {
      final updatedConversations =
          state.conversations.map((conv) {
            if (conv.id == id) {
              return updated;
            }
            return conv;
          }).toList();

      state = ConversationState(
        conversations: updatedConversations,
        folderMap: _organizeFolders(updatedConversations),
      );
    } else {
      print('Failed to update title locally - null response');
    }

    await apiService.updateConversationTitle(id, title);
  }

  Future<void> updateConversationFolder(String id, String folder) async {
    final updated = await ConversationStorage.updateConversationFolder(
      id,
      folder,
    );
    if (updated != null) {
      final updatedConversations =
          state.conversations.map((conv) {
            if (conv.id == id) {
              return updated;
            }
            return conv;
          }).toList();

      state = ConversationState(
        conversations: updatedConversations,
        folderMap: _organizeFolders(updatedConversations),
      );
    }
    await apiService.updateConversationFolder(id, folder);
  }

  // Delete conversation
  Future<void> deleteConversation(String id) async {
    await ConversationStorage.deleteConversation(id);
    final updatedConversations =
        state.conversations.where((conv) => conv.id != id).toList();

    state = ConversationState(
      conversations: updatedConversations,
      folderMap: _organizeFolders(updatedConversations),
    );

    await apiService.deleteConversation(id);
  }

  // Delete all conversations
  Future<void> deleteAllConversations() async {
    _isInitialized = false;
    await ConversationStorage.deleteAllConversations();

    state = ConversationState(conversations: [], folderMap: {});
  }

  // create a new conversation
  Future<Conversation> createConversation({
    required String id,
    required String title,
    required ModelSelected modelSelected,
    MiniApp? miniApp,
  }) async {
    final newConv = Conversation(
      id: id,
      title: title,
      modelSelected: modelSelected,
      messages: [],
      lastMessageAt: DateTime.now(),
      miniApp: miniApp,
    );
    await ConversationStorage.saveConversation(newConv);

    final updatedConversations = [newConv, ...state.conversations];

    state = ConversationState(
      conversations: updatedConversations,
      folderMap: _organizeFolders(updatedConversations),
    );

    return newConv;
  }

  Future<Conversation?> getConversation(String id) async {
    try {
      return state.conversations.firstWhere((conv) => conv.id == id);
    } catch (e) {
      // Return null if not found
      return null;
    }
  }

  @override
  void dispose() {
    // Clean up subscription when notifier is disposed
    _authSubscription?.cancel();
    super.dispose();
  }
}

// Create a simple provider to access sorted folders
final sortedFoldersProvider =
    Provider.family<List<Map<String, dynamic>>, String?>((ref, searchQuery) {
      final state = ref.watch(conversationsProvider);
      final folderMap = state.folderMap;

      // Sort folder names
      final sortedFolderNames =
          folderMap.keys.toList()..sort((a, b) {
            if (a == "") return 1;
            if (b == "") return -1;
            if (a == "Pinned") return -1;
            if (b == "Pinned") return 1;
            return a.compareTo(b);
          });

      return sortedFolderNames
          .map((folderName) {
            final conversations = folderMap[folderName]!;

            // Apply search filter if needed
            final filteredConversations =
                (searchQuery != null && searchQuery.length >= 3)
                    ? conversations
                        .where(
                          (conv) => conv.title.toLowerCase().contains(
                            searchQuery.toLowerCase(),
                          ),
                        )
                        .toList()
                    : conversations;

            return {'name': folderName, 'conversations': filteredConversations};
          })
          // Filter out empty folders when searching
          .where(
            (folder) =>
                searchQuery == null ||
                searchQuery.length < 3 ||
                (folder['conversations'] as List).isNotEmpty,
          )
          .toList();
    });

final conversationsProvider =
    StateNotifierProvider<ConversationsNotifier, ConversationState>((ref) {
      return ConversationsNotifier();
    });
