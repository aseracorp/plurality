import 'package:hive_ce/hive.dart';
import 'package:hive_ce_flutter/hive_flutter.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';
import 'package:path_provider/path_provider.dart';
import 'dart:io';
import 'package:flutter/foundation.dart';

import '../utils/types.dart';
import '../auth/auth-service.dart';

class ConversationStorage {
  static const String _conversationBoxName = 'conversations';

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
    Hive.registerAdapter(MiniAppAdapter());
    Hive.registerAdapter(MiniAppInputAdapter());
    Hive.registerAdapter(ToolCallAdapter());

    // Open box
    if (!kIsWeb) {
      try {
        await Hive.openBox<Conversation>(_conversationBoxName);
      } catch (e) {
        final appDocumentDirectory = await getApplicationSupportDirectory();
        // wait 1 sec and try again
        await Future.delayed(Duration(seconds: 1));

        print('Storage - init - Error opening box $_conversationBoxName');
        // delete the box and try again
        if (Hive.isBoxOpen(_conversationBoxName)) {
          await Hive.box(_conversationBoxName).close();
        }

        String? hivePath =
            appDocumentDirectory.path + '/$_conversationBoxName.hive';

        if (hivePath != null) {
          print('hivePath: $hivePath');
          final file = File(hivePath);
          if (await file.exists()) {
            await file.delete();
            print('Successfully deleted hive file');
          } else {
            print('File does not exist at path: $hivePath');
          }

          // Delete the lock file too
          final lockFile = File('$hivePath.lock');
          if (await lockFile.exists()) {
            await lockFile.delete();
            print('Successfully deleted hive.lock file');
          }
        } else {
          print('hivePath is null');
        }

        await Hive.openBox<Conversation>(_conversationBoxName);
      }
    } else {
      print('Storage - init - Hive initialized for web');

      // Delete the box completely
      await Hive.deleteBoxFromDisk(_conversationBoxName);

      // Additional cleanup for indexedDB
      await Hive.deleteFromDisk();

      await Hive.openBox<Conversation>(_conversationBoxName);
    }
  }

  // Get the box
  static Box<Conversation> _getBox() {
    return Hive.box<Conversation>(_conversationBoxName);
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
        await box.put(conversation.id, conversation);
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
    required String? icon,
    required ModelSelected? modelSelected,
  }) async {
    final conversation = getConversation(conversationId);
    if (conversation == null) return null;

    if (title != "" && title != null) conversation.title = title;

    if (icon != "" && icon != null) conversation.icon = icon;

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

  // set title
  static Future<Conversation?> updateConversationTitle(
    String id,
    String title,
  ) async {
    final conversation = getConversation(id);
    if (conversation == null) return null;

    conversation.title = title;

    await saveConversation(conversation);

    return conversation;
  }

  // set folder
  static Future<Conversation?> updateConversationFolder(
    String id,
    String folder,
  ) async {
    final conversation = getConversation(id);
    if (conversation == null) return null;

    conversation.folder = folder;

    await saveConversation(conversation);

    return conversation;
  }
}
