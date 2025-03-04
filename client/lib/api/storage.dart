import 'package:hive/hive.dart';
import 'package:hive_flutter/hive_flutter.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';
import 'package:path_provider/path_provider.dart';
import 'dart:io';

import '../utils/types.dart';
import '../auth/auth-service.dart';

class ConversationStorage {
  static const String _conversationBoxName = 'conversations';
  static const String _prefBoxName = 'preferences';

  // Initialize Hive
  static Future<void> init() async {
    try {
      final appDocumentDirectory = await getApplicationSupportDirectory();

      print(
        'Storage - init - Initializing Hive at ${appDocumentDirectory.path}',
      );

      Hive.init(appDocumentDirectory.path);
    } catch (e) {
      Hive.initFlutter();
    }

    // Register adapters
    Hive.registerAdapter(MessageContentURLAdapter());
    Hive.registerAdapter(MessageContentAdapter());
    Hive.registerAdapter(MessageAdapter());
    Hive.registerAdapter(ConversationAdapter());
    Hive.registerAdapter(AttachmentAdapter());
    Hive.registerAdapter(ModelSelectedAdapter());
    Hive.registerAdapter(ModelAdapter());

    // Open box
    await Hive.openBox<Conversation>(_conversationBoxName);
    await Hive.openBox<dynamic>(_prefBoxName);
  }

  // Get the box
  static Box<Conversation> _getBox() {
    return Hive.box<Conversation>(_conversationBoxName);
  }

  // Get preferences
  static Box<dynamic> _getPrefBox() {
    return Hive.box<dynamic>(_prefBoxName);
  }

  // Get Selected Model Preference
  static ModelSelected getSelectedModel() {
    final box = _getPrefBox();
    final modelSelected = box.get('modelSelected');

    if (modelSelected == null) {
      return ModelSelected();
    }

    return modelSelected;
  }

  // Save Selected Model Preference
  static Future<void> saveSelectedModel(ModelSelected modelSelected) async {
    final box = _getPrefBox();
    await box.put('modelSelected', modelSelected);
  }

  // CRUD OPERATIONS

  // Get all conversations
  static List<Conversation> getAllConversations() {
    final box = _getBox();
    final list = box.values.toList();
    list.sort((a, b) => b.lastMessageAt.compareTo(a.lastMessageAt));
    return list;
  }

  // Get conversation by ID
  static Conversation? getConversation(String id) {
    final box = _getBox();
    return box.get(id);
  }

  // Save a conversation
  static Future<void> saveConversation(Conversation conversation) async {
    final box = _getBox();
    await box.put(conversation.id, conversation);
  }

  static Future<void> mergeConversations(
    List<Conversation> conversations,
  ) async {
    final box = _getBox();
    for (var conversation in conversations) {
      if (!box.containsKey(conversation.id)) {
        await box.put(conversation.id, conversation);
      } else {
        // print(
        //   'Storage - mergeConversations - Conversation already exists ${conversation.id}',
        // );
      }
    }

    // remove conversations that are not in the new list
    for (var key in box.keys) {
      if (!conversations.any((conv) => conv.id == key)) {
        await box.delete(key);
      }
    }
  }

  // Add a message to a conversation
  static Future<Conversation?> addMessage({
    required String conversationId,
    required Message message,
  }) async {
    final conversation = getConversation(conversationId);
    if (conversation == null) {
      print('Storage - addMessage - Conversation not found $conversationId');
      return null;
    }

    conversation.messages.add(message);
    conversation.lastMessageAt = DateTime.now();

    print('Storage - addMessage - Saving conversation $conversationId');

    await saveConversation(conversation);
    return conversation;
  }

  // Update conversation title
  static Future<Conversation?> updateConversationMetaData({
    required String conversationId,
    required String? title,
    required ModelSelected? modelSelected,
  }) async {
    final conversation = getConversation(conversationId);
    if (conversation == null) return null;

    if (title != "" && title != null) conversation.title = title;

    if (modelSelected != null) conversation.modelSelected = modelSelected;

    await saveConversation(conversation);
    return conversation;
  }

  // Delete a conversation
  static Future<void> deleteConversation(String id) async {
    final box = _getBox();
    await box.delete(id);
  }

  // Delete all conversations
  static Future<void> deleteAllConversations() async {
    final box = _getBox();
    await box.clear();
  }
}
