import 'dart:async';
import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter_riverpod/flutter_riverpod.dart';

import './storage.dart';
import './api.dart';
import './chat_service.dart';
import '../utils/types.dart';
import '../auth/auth-service.dart';

class ConversationsState {
  final List<Conversation> conversations;
  final Map<String, List<Conversation>> folderMap;

  ConversationsState({required this.conversations, required this.folderMap});
}

class FolderData {
  final String name;
  final List<Conversation> conversations;

  FolderData({required this.name, required this.conversations});
}

// In service.dart, modify the ConversationsNotifier class
class ConversationsNotifier extends StateNotifier<ConversationsState> {
  final AuthService _authService = AuthService();
  StreamSubscription? _authSubscription;
  final apiService = ApiService();

  static bool _isInitialized = false;

  ConversationsNotifier()
    : super(ConversationsState(conversations: [], folderMap: {})) {
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
    // Also refresh tool metadata cache
    ChatService().refreshToolMetadata();
    return Future.value();
  }

  // Helper to organize conversations by folder
  Map<String, List<Conversation>> _organizeFolders(
    List<Conversation> conversations,
  ) {
    Map<String, List<Conversation>> folderMap = {};

    for (var conv in conversations) {
      String folderName = conv.folder ?? '';

      if (!folderMap.containsKey(folderName)) {
        folderMap[folderName] = [];
      }

      folderMap[folderName]!.add(conv);
    }

    return folderMap;
  }

  void _loadConversations() async {
    final conversations = await ConversationStorage.getAllConversations();
    state = ConversationsState(
      conversations: conversations,
      folderMap: _organizeFolders(conversations),
    );
  }

  /// Refresh the conversation list.
  ///
  /// Important: the local in-memory `state.conversations` is the source of
  /// truth the UI watches. On web, Hive now stores metadata only (no messages
  /// / blobs), so there is nothing extra to load from it — the old
  /// implementation re-read + re-deserialized the entire Hive box on EVERY
  /// refresh and on EVERY conversation open, which is the seconds-long
  /// click-to-open stall and a big chunk of the GC/GPU jank. We keep the
  /// in-memory list and only swap in the freshly-fetched server metadata.
  Future<void> _loadConversationsFromServer() async {
    final newConv = await apiService.getConversations();
    await ConversationStorage.mergeConversations(newConv);
    if (kIsWeb) {
      // Web: in-memory list is authoritative; Hive is metadata cache only.
      if (newConv.isNotEmpty) {
        state = ConversationsState(
          conversations: newConv,
          folderMap: _organizeFolders(newConv),
        );
      }
      return;
    }
    _loadConversations();
  }

  void loadConversation(String id) async {
    if (id == '') {
      return;
    }

    var conv = await apiService.getConversation(id);
    if (conv != null) {
      await ConversationStorage.saveConversation(conv);
      if (kIsWeb) {
        // Update the in-memory list with this conversation's fresh messages
        // without re-reading the whole Hive box.
        final updatedConversations = state.conversations.map((c) {
          if (c.id == id) return conv!;
          return c;
        }).toList();
        state = ConversationsState(
          conversations: updatedConversations,
          folderMap: _organizeFolders(updatedConversations),
        );
      } else {
        _loadConversations();
      }
      // The freshly-loaded messages might end on an unanswered client tool
      // call (previous client crashed / was closed mid-execution). Hand it
      // to ChatService which guards against double-dispatch via the lock,
      // active-stream, and in-flight-resume checks.
      ChatService().resumeTrailingClientToolsIfNeeded(id);
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

      state = ConversationsState(
        conversations: updatedConversations,
        folderMap: _organizeFolders(updatedConversations),
      );
    }
  }

  // Update title
  Future<void> updateConversationMetaData({
    required String conversationId,
    String? title,
    String? icon,
    ModelSelected? modelSelected,
  }) async {
    final updated = await ConversationStorage.updateConversationMetaData(
      conversationId: conversationId,
      title: title,
      icon: icon,
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

      state = ConversationsState(
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

      state = ConversationsState(
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

      state = ConversationsState(
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

    state = ConversationsState(
      conversations: updatedConversations,
      folderMap: _organizeFolders(updatedConversations),
    );

    await apiService.deleteConversation(id);
  }

  // Delete all conversations
  Future<void> deleteAllConversations() async {
    _isInitialized = false;
    await ConversationStorage.deleteAllConversations();

    state = ConversationsState(conversations: [], folderMap: {});
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

    state = ConversationsState(
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

// Ordered list of conversation IDs from the last server search.
// null means no active search; empty list means search returned nothing.
final searchResultIdsProvider = StateProvider<List<String>?>((ref) => null);

// Session-only: when false, conversations with a non-empty trigger_id are
// hidden from the sidebar and from search results.
final showHiddenConversationsProvider = StateProvider<bool>((ref) => false);

// Mirrors ChatService().conversationStatuses (a ValueNotifier) into Riverpod so
// providers can react to live state changes. The map only contains entries for
// conversations currently in a non-idle state, so membership == "active now".
final liveConversationStatusesProvider =
    StreamProvider<Map<String, ConversationStatus>>((ref) {
      final notifier = ChatService().conversationStatuses;
      final controller =
          StreamController<Map<String, ConversationStatus>>();
      void listener() => controller.add(notifier.value);
      notifier.addListener(listener);
      controller.add(notifier.value);
      ref.onDispose(() {
        notifier.removeListener(listener);
        controller.close();
      });
      return controller.stream;
    });

// Create a simple provider to access sorted folders
final sortedFoldersProvider =
    Provider.family<List<Map<String, dynamic>>, String?>((ref, searchQuery) {
      final state = ref.watch(conversationsProvider);
      final folderMap = state.folderMap;
      final searchIds = ref.watch(searchResultIdsProvider);
      final showHidden = ref.watch(showHiddenConversationsProvider);
      final liveStatuses =
          ref.watch(liveConversationStatusesProvider).valueOrNull ?? const {};

      // A triggered conversation should still appear when it has activity:
      // either persisted non-idle state (e.g. waitingForApproval) or a live
      // entry in the global status stream (processing / waiting_for_tool).
      bool isVisible(Conversation c) =>
          showHidden || !c.isHidden || liveStatuses.containsKey(c.id);

      // Move each sub-agent row directly under its parent (preserving sibling
      // order). Sub-agents whose parent isn't in the same visible list fall
      // through to the end of the list in their original order.
      List<Conversation> nestSubAgents(List<Conversation> convs) {
        final parents = <Conversation>[];
        final children = <String, List<Conversation>>{};
        final orphans = <Conversation>[];
        final parentIds = <String>{};
        for (final c in convs) {
          if (c.triggerType != 'sub_agent') {
            parents.add(c);
            parentIds.add(c.id);
          }
        }
        for (final c in convs) {
          if (c.triggerType != 'sub_agent') continue;
          final pid = c.triggerId;
          if (pid != null && parentIds.contains(pid)) {
            children.putIfAbsent(pid, () => []).add(c);
          } else {
            orphans.add(c);
          }
        }
        final out = <Conversation>[];
        for (final p in parents) {
          out.add(p);
          final kids = children[p.id];
          if (kids != null) out.addAll(kids);
        }
        out.addAll(orphans);
        return out;
      }

      final isSearching = searchQuery != null && searchQuery.length >= 3 && searchIds != null;

      // When searching, return a single flat list ordered by server ranking
      if (isSearching) {
        // Build a lookup of all conversations by ID
        final allConvs = <String, Conversation>{};
        for (final convs in folderMap.values) {
          for (final conv in convs) {
            allConvs[conv.id] = conv;
          }
        }

        // Return in server-ranked order
        final ranked = searchIds
            .where((id) => allConvs.containsKey(id))
            .map((id) => allConvs[id]!)
            .where(isVisible)
            .toList();

        if (ranked.isEmpty) return [];
        return [{'name': 'Search Results', 'conversations': nestSubAgents(ranked)}];
      }

      // Normal view: group by folders
      final sortedFolderNames =
          folderMap.keys.toList()..sort((a, b) {
            if (a == "") return 1;
            if (b == "") return -1;
            if (a == "Pinned") return -1;
            if (b == "Pinned") return 1;
            return a.compareTo(b);
          });

      final result = <Map<String, dynamic>>[];
      for (final folderName in sortedFolderNames) {
        final conversations =
            folderMap[folderName]!.where(isVisible).toList();
        if (conversations.isEmpty) continue;
        result.add({'name': folderName, 'conversations': nestSubAgents(conversations)});
      }
      return result;
    });

final conversationsProvider =
    StateNotifierProvider<ConversationsNotifier, ConversationsState>((ref) {
      return ConversationsNotifier();
    });
