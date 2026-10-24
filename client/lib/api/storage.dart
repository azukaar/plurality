import 'package:hive_ce/hive.dart';
import 'package:hive_ce_flutter/hive_flutter.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path_provider/path_provider.dart';
import 'dart:io';
import 'package:flutter/foundation.dart';

import '../utils/types.dart';

class ConversationStorage {
  static const String _conversationBoxName = 'conversations';

  /// On web, Hive keeps only a lightweight metadata snapshot (no messages, no
  /// base64 image blobs) in this box. The server is the source of truth for
  /// message history; persisting full conversations to IndexedDB made every
  /// open re-read + re-deserialize the ENTIRE history of every conversation
  /// synchronously on the UI isolate — the seconds-long click-to-open delay
  /// and the jank/GC/GPU crashes (a cold tab was faster precisely because
  /// it had less cached data to read back).
  static const String _webMetaBoxName = 'conversations_meta';

  static bool get _useMetaOnly => kIsWeb;

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
    Hive.registerAdapter(ContentImageURLAdapter());
    Hive.registerAdapter(ContentPartAdapter());
    Hive.registerAdapter(FunctionCallDataAdapter());
    Hive.registerAdapter(ToolCallAdapter());
    Hive.registerAdapter(MessageAdapter());
    Hive.registerAdapter(ConversationAdapter());
    Hive.registerAdapter(AttachmentAdapter());
    Hive.registerAdapter(ModelSelectedAdapter());
    Hive.registerAdapter(ModelAdapter());
    Hive.registerAdapter(MiniAppAdapter());
    Hive.registerAdapter(MiniAppInputAdapter());
    Hive.registerAdapter(ClientLockAdapter());

    if (!kIsWeb) {
      try {
        await Hive.openBox<Conversation>(_conversationBoxName);
      } catch (e) {
        final appDocumentDirectory = await getApplicationSupportDirectory();
        // wait 1 sec and try again
        await Future.delayed(const Duration(seconds: 1));

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
      print('Storage - init - Hive initialized for web (metadata-only mode)');

      // Clean any legacy full-message box from previous versions. It holds
      // every conversation's messages + base64 image blobs in IndexedDB;
      // we no longer keep it in memory on web.
      try {
        await Hive.deleteBoxFromDisk(_conversationBoxName);
      } catch (_) {}

      // (Re)open the lightweight metadata box.
      await Hive.openBox<Conversation>(_webMetaBoxName);
    }
  }

  // Get the box
  static Box<Conversation> _getBox() {
    if (_useMetaOnly) return Hive.box<Conversation>(_webMetaBoxName);
    return Hive.box<Conversation>(_conversationBoxName);
  }

  /// Strip message payloads (incl. base64 image blobs) before persisting a
  /// conversation on web. The in-memory list keeps the full messages for the
  /// current session; the persisted copy is metadata only.
  static Conversation _metaOnly(Conversation conversation) {
    return Conversation(
      id: conversation.id,
      title: conversation.title,
      lastMessageAt: conversation.lastMessageAt,
      modelSelected: conversation.modelSelected,
      createdAt: conversation.createdAt,
      messages: const [],
      miniApp: conversation.miniApp,
      folder: conversation.folder,
      icon: conversation.icon,
      stateString: conversation.stateString,
      triggerId: conversation.triggerId,
      triggerType: conversation.triggerType,
    );
  }

  // CRUD OPERATIONS

  // Get all conversations (metadata only on web)
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
    // On web, persist only metadata so IndexedDB never holds message blobs.
    if (_useMetaOnly) {
      await box.put(conversation.id, _metaOnly(conversation));
      return;
    }
    await box.put(conversation.id, conversation);
  }

  /// Merge server-provided conversation list into local storage.
  /// Web: merge only the lightweight metadata; message histories are fetched
  /// from the server per open and never persisted.
  static Future<void> mergeConversations(
    List<Conversation> conversations,
  ) async {
    final box = _getBox();
    for (var conversation in conversations) {
      if (_useMetaOnly) {
        conversation = _metaOnly(conversation);
      } else if (box.containsKey(conversation.id)) {
        // Preserve local messages when the server response has none
        // (the list endpoint strips messages for efficiency)
        final existing = box.get(conversation.id)!;
        if (conversation.messages.isEmpty && existing.messages.isNotEmpty) {
          conversation.messages = existing.messages;
        }
      }
      await box.put(conversation.id, conversation);
    }

    // remove conversations that are not in the new list
    for (var key in box.keys.toList()) {
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

    if (title != '' && title != null) conversation.title = title;

    if (icon != '' && icon != null) conversation.icon = icon;

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
