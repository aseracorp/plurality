import 'dart:async';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import './storage.dart';
import './api.dart';
import '../utils/types.dart';
import '../auth/auth-service.dart';

// Provider for current user ID

// Provider for all conversations
final conversationsProvider =
    StateNotifierProvider<ConversationsNotifier, List<Conversation>>((ref) {
      return ConversationsNotifier();
    });

// Notifier class
class ConversationsNotifier extends StateNotifier<List<Conversation>> {
  final AuthService _authService = AuthService();
  StreamSubscription? _authSubscription;
  final apiService = ApiService();

  ConversationsNotifier() : super([]) {
    // Load initial local data
    _loadConversations();

    // Subscribe to auth changes
    _authSubscription = _authService.authStateChanges.listen((user) {
      if (user != null) {
        print('User logged in, loading conversations');
        Future.microtask(() {
          refresh();
        });
      }
    });
  }

  void _loadConversations() async {
    state = await ConversationStorage.getAllConversations();
  }

  // merge new conversations with existing ones by only setting the new ones
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
      state =
          state.map((conv) {
            if (conv.id == conversationId) {
              return updated;
            }
            return conv;
          }).toList();
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
      state =
          state.map((conv) {
            if (conv.id == conversationId) {
              return updated;
            }
            return conv;
          }).toList();
    }
  }

  // Delete conversation
  Future<void> deleteConversation(String id) async {
    await ConversationStorage.deleteConversation(id);
    state = state.where((conv) => conv.id != id).toList();
    await apiService.deleteConversation(id);
  }

  // Delete all conversations
  Future<void> deleteAllConversations() async {
    await ConversationStorage.deleteAllConversations();
    state = [];
  }

  // Refresh data
  Future<void> refresh() async {
    await _loadConversationsFromServer();
    return Future.value();
  }

  // create a new conversation
  Future<Conversation> createConversation({
    required String id,
    required String title,
    required ModelSelected modelSelected,
  }) async {
    final newConv = Conversation(
      id: id,
      title: title,
      modelSelected: modelSelected,
      messages: [],
      lastMessageAt: DateTime.now(),
    );
    await ConversationStorage.saveConversation(newConv);
    state = [newConv, ...state];
    return newConv;
  }

  Future<Conversation?> getConversation(String id) async {
    try {
      return state.firstWhere((conv) => conv.id == id);
    } catch (e) {
      // Return null if not found
      return null;
    }
  }

  // Get Selected Model Preference
  ModelSelected getSelectedModel() {
    return ConversationStorage.getSelectedModel();
  }

  void saveSelectedModel(ModelSelected modelSelected) {
    ConversationStorage.saveSelectedModel(modelSelected);
  }

  @override
  void dispose() {
    // Clean up subscription when notifier is disposed
    _authSubscription?.cancel();
    super.dispose();
  }
}
