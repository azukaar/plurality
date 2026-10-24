import 'package:flutter/foundation.dart'
    show ValueNotifier, kIsWeb;
import 'package:plurality/chat/message-list/attachments.dart';
import 'package:url_launcher/url_launcher_string.dart';
import '../snackbar.dart';
import 'package:flutter/material.dart';
import 'dart:convert';
import '../../utils/types.dart';
import '../../utils/index.dart' show formatToolDisplayName;
import '../../api/api.dart';
import '../../api/service.dart';
import '../../api/chat_service.dart';
import '../../api/tts.dart';
import '../../api/mini-apps.dart';
import '../../api/models_service.dart';
import '../../api/preferences_provider.dart';
import '../../api/client_identity.dart';
import '../../preset/preset-editor.dart';
import 'AnimatedMessageBox.dart';
import 'package:image_picker/image_picker.dart';
import 'package:file_picker/file_picker.dart';
import 'package:cross_file/cross_file.dart';
import 'package:mime/mime.dart';
import 'package:top_snackbar_flutter/top_snack_bar.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'image.dart';
import 'input.dart';
import '../../utils/index.dart';
import '../../utils/file-types.dart';
import '../../api/stt.dart';
import 'minimap.dart';
import '../miniapps.dart';
import './MiniAppForm.dart';
import 'middle-click.dart';
import 'package:super_sliver_list/super_sliver_list.dart';
import 'toolcall-badge.dart';
import 'fs_read_attach.dart' show isFsReadAttachCall;
import 'fs_write_diff.dart';
import 'wait_badge.dart' show isHiddenWaitResume, isHiddenWaitResumeMessage;

class ChatInterface extends ConsumerStatefulWidget {
  final String conversationId;
  final VoidCallback? onConversationUpdated;
  final String initialMessage;
  final bool isMobile;
  final Function updateMainTitle;
  final Function(String, bool)? setConversationID;

  const ChatInterface({
    super.key,
    required this.conversationId,
    this.onConversationUpdated,
    this.initialMessage = '',
    this.setConversationID,
    required this.isMobile,
    required this.updateMainTitle,
  });

  @override
  ConsumerState<ChatInterface> createState() => _ChatInterfaceState();
}

class _ChatInterfaceState extends ConsumerState<ChatInterface>
    with WidgetsBindingObserver {
  final ApiService _apiService = ApiService();
  final ChatService _chatService = ChatService();
  final FocusNode _inputFocusNode = FocusNode();
  final GlobalKey<FormState> _formKey = GlobalKey<FormState>();
  final TextEditingController _messageController = TextEditingController();

  // Main content scroll controller
  final ScrollController _mainScrollController = ScrollController();

  // MiniMap scroll controller
  final ScrollController _miniMapScrollController = ScrollController();

  // List controllers for item-level scrolling (super_sliver_list)
  final ListController _listController = ListController();
  final ListController _miniMapListController = ListController();

  final ImagePicker _imagePicker = ImagePicker();

  final List<Attachment> attachments = [];

  bool _isNearBottom = true;
  bool _needsBottomMargin = false;
  bool _closeMessageWarning = false;
  bool _didInitialScroll = false;

  // Last ConversationState seen by _onSessionChanged. Used to rebuild the
  // chat only on state transitions (stream start/end, tool-wait, error)
  // instead of on every streaming token.
  ConversationState _lastSessionState = ConversationState.idle;

  // Last (state|activity|toolName) key and processing flag for THIS
  // conversation from the global status stream. The status notifier fires on
  // every status event for ANY conversation of the user; these track whether
  // the open chat's own status actually changed before triggering a rebuild.
  String _lastStatusKey = '';
  bool _lastStatusProcessing = false;

  // Mirrors the main list's itemCount from the most recent buildMessageList.
  // jumpToItem callers must use this — recomputing a filter here drifts and
  // throws an out-of-bounds assertion in super_sliver_list.
  int _lastMainItemCount = 0;

  ModelSelected _modelSelected = ModelSelected();

  MiniApp? _miniAppSelected;

  String _miniAppPrePrompt = '';

  /// Observable session state from ChatService.
  late ValueNotifier<ChatSessionState> _session;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _mainScrollController.addListener(_scrollListener);

    _miniAppPrePrompt = '';

    // Initialize the session notifier from ChatService
    _session = _chatService.getSession(widget.conversationId);
    _session.addListener(_onSessionChanged);

    // Sub-agents and other LLM-spawned conversations never populate
    // _session.state on this client; their busy state arrives via the global
    // status stream, so we mirror it into rebuilds.
    _chatService.conversationStatuses.addListener(_onStatusChanged);

    final preferencesNotifier = ref.read(preferencesProvider.notifier);
    preferencesNotifier.loadAllPreferences().then((_) {
      _updateSelectedModel();
    });

    _updateSelectedModel();
    _loadConversation(widget.conversationId);

    // Drop any stale streaming items left behind by a previous interrupted
    // run on this conversation, so the freshly-loaded DB messages are the
    // only thing rendered. No-op when there's a live SSE attached.
    _chatService.resetSessionIfIdle(widget.conversationId);

    // Reattach to any live SSE stream for this conversation. Recovers from
    // a dropped socket (phone lock, network blip) where the UI is otherwise
    // stuck on "loading" — no-op for new (empty id) conversations and
    // idempotent when a stream is already attached.
    _chatService.reconnect(widget.conversationId);

    // Ensure ChatService has a reference to the conversations notifier
    _chatService.setConversationsNotifier(
      ref.read(conversationsProvider.notifier),
    );

    // Send initial message if provided
    if (widget.initialMessage.isNotEmpty) {
      Future.delayed(Duration(milliseconds: 500), () {
        _submitMessage(widget.initialMessage);
      });
    }
  }

  String get miniAppName {
    if (_miniAppSelected != null) {
      return _miniAppSelected!.name;
    } else {
      return '';
    }
  }

  @override
  void didUpdateWidget(ChatInterface oldWidget) {
    super.didUpdateWidget(oldWidget);

    // Check if the conversationId has changed
    if (oldWidget.conversationId != widget.conversationId) {
      // Only clear the form-generated pre-prompt when actually switching
      // conversations. Resetting it on every rebuild wiped the prompt before
      // the user could send it.
      _miniAppPrePrompt = '';

      if (oldWidget.conversationId != "") {
        // If the conversationId has changed, stop TTS and load the new conversation
        TTSService().stop();
      }

      // Switch session listener to the new conversation
      _session.removeListener(_onSessionChanged);
      _session = _chatService.getSession(widget.conversationId);
      _session.addListener(_onSessionChanged);

      _updateSelectedModel();
      _loadConversation(widget.conversationId);

      // Drop stale streaming items left over from a previous interrupted run.
      _chatService.resetSessionIfIdle(widget.conversationId);

      // Reattach to any live SSE stream for the newly-selected conversation.
      _chatService.reconnect(widget.conversationId);

      setState(() {
        _needsBottomMargin = false;
        _closeMessageWarning = false;
        _didInitialScroll = false;
        // Reset the transition tracker to the newly-bound session's current
        // state so the next real transition still triggers a rebuild.
        _lastSessionState = _session.value.state;
        // Reset status-trackers so the newly-selected conversation's current
        // status (if any) triggers a rebuild on the next status event.
        _lastStatusKey = '';
        _lastStatusProcessing = false;
      });
    }
  }

  /// Called when the global status stream changes. The stream fires for EVERY
  /// conversation of this user (sub-agents, background tasks, other tabs) and
  /// each event installs a NEW map on the shared ValueNotifier — so this
  /// listener runs for all of them, not just this chat's. A full setState()
  /// per event rebuilt the entire conversation (every DB message's markdown
  /// + the minimap + composer) mid-typing whenever ANY other conversation
  /// transitioned, which showed up as input lag and jank.
  ///
  /// Rebuild only when THIS conversation's status actually changed: the
  /// (state|activity|toolName) key or the processing flag. Duplicate events
  /// for this conversation (server re-broadcasts an unchanged typing status,
  /// or a ModelSelected-only refresh with no status change) are filtered, and
  /// events for other conversations no longer touch this widget. Real SSE
  /// state flips are still handled by _onSessionChanged's transition guard.
  void _onStatusChanged() {
    if (!mounted) return;
    final status =
        _chatService.conversationStatuses.value[widget.conversationId];
    final isProcessing = status?.isProcessing ?? false;
    final statusKey = status == null
        ? ''
        : '${status.state}|${status.activity}|${status.toolName}';
    final keyChanged = statusKey != _lastStatusKey;
    final processingChanged = isProcessing != _lastStatusProcessing;
    if (!keyChanged && !processingChanged) return;
    _lastStatusKey = statusKey;
    _lastStatusProcessing = isProcessing;
    setState(() {});
  }

  /// Called whenever ChatService session state changes.
  void _onSessionChanged() {
    if (!mounted) return;

    final state = _session.value;

    // Handle errors from ChatService
    if (state.error != null && state.error!.isNotEmpty) {
      showTopSnackBar(
        Overlay.of(context),
        PrettySnackbar(message: state.error!, type: SnackbarType.error),
      );
    }

    // Focus the input when streaming ends (skip on mobile to avoid virtual keyboard pop-up)
    if (state.state != ConversationState.processing) {
      if (!widget.isMobile) {
        Future.delayed(Duration(milliseconds: 150), () {
          if (mounted) _inputFocusNode.requestFocus();
        });
      }
    }

    // The session notifier fires on EVERY SSE token while text streams. A
    // full setState here would rebuild the entire chat — every rendered DB
    // message (each an AnimatedMessageBox with a markdown subtree), the
    // minimap, the composer — per token: O(history) markdown re-renders that
    // showed up as GPU stalls and Firefox crashes. The streaming slot is
    // wrapped in its own ValueListenableBuilder below and re-renders itself
    // per token; this whole-chat rebuild only needs to happen on state
    // transitions (stream start/end, tool-wait, error).
    if (state.state != _lastSessionState || state.error != null) {
      _lastSessionState = state.state;
      setState(() {});
    }
  }

  // Extract the model selection logic to a separate method
  void _updateSelectedModel() {
    final conversationsState = ref.read(conversationsProvider);
    final preferences = ref.read(preferencesProvider);

    // Try to get the model from the specific conversation first
    final matches =
        conversationsState.conversations
            .where((conv) => conv.id == widget.conversationId)
            .toList();

    if (matches.isNotEmpty) {
      // Use the model from the conversation
      _modelSelected = matches.first.modelSelected;
      _miniAppSelected = matches.first.miniApp;
    } else {
      // Fall back to the globally selected model from preferences.
      // Free-plan users get the Fast preset auto-applied the first time the
      // model picker opens (see _checkAndSetDefaultModels in model-picker.dart).
      _modelSelected = preferences.selectedModel;
      _miniAppSelected = null;
    }
  }

  // Extract the model selection logic to a separate method
  void _loadConversation(String id) {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    try {
      conversationsNotifier.loadConversation(id);
    } catch (e) {
      SnackBar(
        content: Text('Failed to load conversation: $e'),
        showCloseIcon: true,
      );
    }
  }

  void _setSelectedModel(ModelSelected modelSelected) {
    setState(() {
      _modelSelected = modelSelected;
    });

    if (widget.conversationId.isEmpty) {
      final preferencesNotifier = ref.read(preferencesProvider.notifier);
      preferencesNotifier.setSelectedModel(modelSelected);
    } else {
      // Mirror the user-driven change into the conversation in Riverpod
      // (and Hive) so build()'s sync from currentConversation.modelSelected
      // doesn't immediately wipe the edit. The server still sees it only
      // on the next /chat or tool-results POST — round-trips as before.
      ref
          .read(conversationsProvider.notifier)
          .updateConversationMetaData(
            conversationId: widget.conversationId,
            modelSelected: modelSelected,
          );
    }
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _session.removeListener(_onSessionChanged);
    _chatService.conversationStatuses.removeListener(_onStatusChanged);
    _listController.dispose();
    _miniMapListController.dispose();
    _mainScrollController.dispose();
    _miniMapScrollController.dispose();
    _inputFocusNode.dispose();
    // Guard: controller may already be disposed if widget was torn down during navigation
    try { _messageController.dispose(); } catch (_) {}
    // Release the streaming session for this (now closed) conversation so its
    // growing items list is freed. No-op for conversations with a live stream.
    _chatService.disposeSession(widget.conversationId);
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) {
      // Re-attach SSE for the currently-viewed conversation. The OS may have
      // killed the socket while we were backgrounded. reconnect() is a no-op
      // if the stream is already healthy.
      _chatService.reconnect(widget.conversationId);
    }
  }

  double get _contentMaxExtent {
    if (!_mainScrollController.hasClients) return 0;
    final max = _mainScrollController.position.maxScrollExtent;
    if (!_needsBottomMargin) return max;
    final viewportHeight = MediaQuery.of(context).size.height;
    return (max - (viewportHeight - 400) + 16.0).clamp(0.0, max);
  }

  void _scrollListener() {
    if (!_mainScrollController.hasClients) return;
    final isNearBottom =
        _mainScrollController.position.pixels >= _contentMaxExtent - 200;
    if (isNearBottom != _isNearBottom) {
      setState(() {
        _isNearBottom = isNearBottom;
      });
    }
  }

  /// Anchor the main list to its last item. Uses the itemCount from the most
  /// recent build so it matches whatever super_sliver_list is rendering.
  void _scrollToBottom() {
    if (!_listController.isAttached ||
        !_mainScrollController.hasClients ||
        _lastMainItemCount <= 0) return;
    // Clamp to the item count of the most recent build. Streaming content can
    // toggle itemCount between the build and this callback (e.g. a tool event
    // landing while the one-shot initial-scroll frame is in flight): jumping
    // to a stale index throws an out-of-bounds assertion in super_sliver_list
    // and crashes the whole tab.
    final targetIndex = (_lastMainItemCount - 1).clamp(0, _lastMainItemCount - 1);
    _listController.jumpToItem(
      index: targetIndex,
      scrollController: _mainScrollController,
      alignment: 1.0,
    );
  }

  void _handleStop() {
    _chatService.cancel(widget.conversationId);
  }

  Future<void> _editMiniApp(MiniApp miniapp) async {
    final ModelsData modelsData;
    try {
      modelsData = await ModelsService().get();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to load models: $e')),
      );
      return;
    }
    if (!mounted) return;
    final saved = await showDialog<MiniApp>(
      context: context,
      builder: (ctx) => PresetEditorDialog(
        modelsData: modelsData,
        existing: miniapp,
      ),
    );
    if (saved == null) return;
    try {
      await MiniAppsService().updateMiniApp(miniapp.id, saved);
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Failed to save preset: $e')),
      );
    }
  }

  void _removeAttachment(Attachment? attachment) {
    setState(() {
      attachments.remove(attachment);
    });
  }

  void _addAttachment(Attachment attachment) {
    setState(() {
      attachments.add(attachment);
    });
  }

  Future<void> _pickImage({ImageSource source = ImageSource.gallery}) async {
    try {
      final XFile? image = await _imagePicker.pickImage(
        source: source,
        maxWidth: 1024,
        maxHeight: 1024,
        imageQuality: 80,
      );

      if (image != null) {
        final bytes = await image.readAsBytes();
        final mimeType = image.mimeType ?? 'image/jpeg';
        final base64Data = base64Encode(bytes);
        setState(() {
          // TEMP: remove existing image attachments because we only support one image for now
          attachments.removeWhere((a) => a.type == 'image_url');
          attachments.add(
            Attachment(
              type: 'image_url',
              content: 'data:$mimeType;base64,$base64Data',
            ),
          );
        });
      }
    } catch (e) {
      print('Error picking image: $e');
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Failed to pick image'),
          showCloseIcon: true,
        ),
      );
    }
  }

  Future<void> _pickFile() async {
    try {
      final FilePickerResult? result = await FilePicker.platform.pickFiles(
        type: FileType.custom,
        allowedExtensions: textFileExtensions + documentFileExtensions,
      );
      if (result != null) {
        await _attachFile(result.files.single.xFile);
      }
    } catch (e) {
      print('Error picking file: $e');
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Failed to pick file'),
          showCloseIcon: true,
        ),
      );
    }
  }

  /// Single entry point for attaching a local file from any source
  /// (file picker, drag-and-drop, paste). Routes by extension:
  /// image → inline data URI, text/code → inline snippet, otherwise → /upload.
  Future<void> _attachFile(XFile xFile) async {
    final filename = xFile.name;
    final dotIdx = filename.lastIndexOf('.');
    final ext = dotIdx >= 0 ? filename.substring(dotIdx + 1).toLowerCase() : '';

    if (imageFileExtensions.contains(ext)) {
      final bytes = await xFile.readAsBytes();
      final mimeType = lookupMimeType(filename, headerBytes: bytes) ?? 'image/jpeg';
      final base64Data = base64Encode(bytes);
      setState(() {
        // Only one image at a time.
        attachments.removeWhere((a) => a.type == 'image_url');
        attachments.add(
          Attachment(
            type: 'image_url',
            content: 'data:$mimeType;base64,$base64Data',
          ),
        );
      });
      return;
    }

    if (textFileExtensions.contains(ext)) {
      final bytes = await xFile.readAsBytes();
      final text = utf8.decode(bytes);
      setState(() {
        attachments.add(
          Attachment(
            type: 'snippet',
            filename: filename,
            ext: ext,
            content: text,
          ),
        );
      });
      return;
    }

    await _uploadAttachment(xFile, filename, ext);
  }

  /// Upload a non-image, non-text attachment to /upload and add it to the
  /// composer state. Shows a spinner placeholder until the upload settles.
  Future<void> _uploadAttachment(XFile xFile, String filename, String ext) async {
    final attType = documentTypeExts.contains(ext) ? ext : 'file';
    final placeholder = Attachment(
      type: attType,
      filename: filename,
      ext: ext,
      content: '',
      uploading: true,
    );
    setState(() {
      attachments.add(placeholder);
    });

    try {
      final bytes = await xFile.readAsBytes();
      final result = await _apiService.uploadAttachment(
        filename: filename,
        bytes: bytes,
      );
      if (!mounted) return;
      setState(() {
        final idx = attachments.indexOf(placeholder);
        if (idx >= 0) {
          attachments[idx] = placeholder.copyWith(
            type: result.type,
            content: result.url,
            ext: result.ext.isNotEmpty ? result.ext : ext,
            uploading: false,
            clearUploadError: true,
          );
        }
      });
    } catch (e) {
      print('Error uploading attachment: $e');
      if (!mounted) return;
      setState(() {
        final idx = attachments.indexOf(placeholder);
        if (idx >= 0) {
          attachments[idx] = placeholder.copyWith(
            uploading: false,
            uploadError: e.toString(),
          );
        }
      });
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text('Failed to upload "$filename"'),
          showCloseIcon: true,
        ),
      );
    }
  }

  Future<void> _handleSubmit(BuildContext context) async {
    if (_formKey.currentState!.validate()) {
      final prePrompt = _miniAppPrePrompt;
      final text = _messageController.text;
      final userMessage = prePrompt + text;
      _messageController.clear();
      _miniAppPrePrompt = '';
      final ok = await _submitMessage(userMessage);
      if (!ok && mounted) {
        // The send failed (e.g. a rejected model). Restore the composer so the
        // mini-app's form-generated prompt and any typed text survive for a
        // retry — otherwise the form looks filled but its values are gone and
        // an empty message box can no longer be sent.
        setState(() {
          _miniAppPrePrompt = prePrompt;
          if (_messageController.text.isEmpty) _messageController.text = text;
        });
      }
    }
  }

  /// Create a Message from user input and delegate to ChatService.
  /// Returns true when the message was accepted (or is being handled normally),
  /// false when it failed to send so the caller can restore the composer.
  Future<bool> _submitMessage(String? userMessage) async {
    // Refuse to submit while any attachment is still uploading or has errored.
    if (attachments.any((a) => a.uploading)) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Please wait for uploads to finish'),
          showCloseIcon: true,
        ),
      );
      return false;
    }
    if (attachments.any((a) => a.uploadError != null)) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Remove failed uploads before sending'),
          showCloseIcon: true,
        ),
      );
      return false;
    }
    _needsBottomMargin = true;
    final conversationsNotifier = ref.read(conversationsProvider.notifier);

    // Build the user message
    Message? newMessage;
    if ((userMessage != null && userMessage.isNotEmpty) ||
        attachments.isNotEmpty) {
      // Check if there are image attachments
      final imageAttachments =
          attachments.where((a) => a.type == 'image_url').toList();
      final documentAttachments =
          attachments.where((a) => isDocumentType(a.type)).toList();
      final otherAttachments =
          attachments.where((a) => a.type != 'image_url' && !isDocumentType(a.type)).toList();

      List<ContentPart> contentParts = [];

      // Add text content
      if (userMessage != null && userMessage.isNotEmpty) {
        contentParts.add(ContentPart(type: 'text', text: userMessage));
      }

      // Add image attachments as image_url content parts
      for (final img in imageAttachments) {
        contentParts.add(ContentPart(
          type: 'image_url',
          imageUrl: ContentImageURL(url: img.content),
        ));
      }

      // Add document attachments (pdf, docx, xlsx, pptx)
      for (final att in documentAttachments) {
        contentParts.add(ContentPart(type: att.type, text: att.content, filename: att.filename));
      }

      // Add other attachments as text content parts (snippets, files)
      for (final att in otherAttachments) {
        contentParts.add(ContentPart(type: att.type, text: att.content, filename: att.filename));
      }

      newMessage = Message(
        role: 'user',
        content: contentParts,
      );
    }

    if (newMessage == null) return false;

    setState(() {
      attachments.clear();
    });

    final isNew = widget.conversationId.isEmpty;

    // For existing conversations, add the user message to local state immediately.
    if (!isNew) {
      conversationsNotifier.addMessage(
        conversationId: widget.conversationId,
        message: newMessage,
      );

      // Scroll the user's message to the top of the viewport, once per submit.
      // Done here (not on state transitions) so multi-turn tool/AI events
      // don't yank the scroll position around between turns. The bottom margin
      // added above gives the streaming response room to grow downward without
      // pushing the user message off-screen.
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (!mounted ||
            !_listController.isAttached ||
            !_mainScrollController.hasClients) return;

        final conversationsState = ref.read(conversationsProvider);
        final currentConversation = conversationsState.conversations.firstWhere(
          (conv) => conv.id == widget.conversationId,
          orElse: () => Conversation(
            id: widget.conversationId, title: '', messages: [],
            lastMessageAt: DateTime.now(), modelSelected: _modelSelected,
          ),
        );
        final visibleMessages = currentConversation.messages
            .where((m) => m.role != 'tool')
            .toList();
        final lastUserIndex =
            visibleMessages.lastIndexWhere((m) => m.role == 'user');
        if (lastUserIndex < 0) return;

        _listController.jumpToItem(
          index: lastUserIndex,
          scrollController: _mainScrollController,
          alignment: 0.0,
        );
      });
    }

    try {
      // sendMessage returns the conversation ID once the server assigns it.
      // For new conversations this awaits the first SSE event; for existing ones
      // it returns immediately.
      final resolvedId = await _chatService.sendMessage(
        conversationId: widget.conversationId,
        message: newMessage,
        modelSelected: _modelSelected,
        miniApp: _miniAppSelected,
      );

      // For new conversations: navigate to the real conversation and return.
      // ChatService holds the SSE stream — this widget disposes cleanly.
      // Title is generated server-side automatically.
      if (isNew && resolvedId != null && resolvedId.isNotEmpty) {
        if (mounted) widget.setConversationID?.call(resolvedId, true);
        return true;
      }

      // A new conversation that never resolved an id failed to start (e.g. the
      // server rejected the model). Report failure so the composer is restored.
      if (isNew) return false;

      if (!mounted) return true;
      if (!widget.isMobile) {
        Future.delayed(Duration(milliseconds: 150), () {
          if (mounted) _inputFocusNode.requestFocus();
        });
      }
      return true;
    } catch (e, s) {
      print('Error: $e');
      print('Stack: $s');
      if (context.mounted) {
        showTopSnackBar(
          Overlay.of(context),
          PrettySnackbar(message: e.toString(), type: SnackbarType.error),
        );
      }
      return false;
    }
  }

  /// Render a single Message into its content widgets (attachments, text, tool calls).
  /// Used by both streaming and non-streaming paths — single source of truth.
  List<Widget> _buildMessageWidgets({
    required Message message,
    required List<Message> allMessagesForLookup,
    required bool isProcessing,
    required int index,
    required int visibleMessageCount,
    required bool mini,
    bool isLoading = false,
    Set<String> excludeToolIds = const {},
  }) {
    final widgets = <Widget>[];
    final messageText = message.textContent;

    // Attachments (images and other non-text content)
    for (final part in message.content.where((c) => c.type != 'text')) {
      widgets.add(AttachmentViewer(
        mini: mini,
        toolCall: null,
        loading: false,
        attachment: Attachment(
          type: part.type,
          content: (part.type == 'image_url' ? part.imageUrl?.url : part.text) ?? '',
          filename: part.filename,
        ),
        removeAttachment: _removeAttachment,
        editMode: false,
      ));
    }

    // Text content
    final sanitized = sanitizeMessages(messageText);
    if (sanitized != "") {
      widgets.add(AnimatedMessageBox(
        iconURL: _miniAppSelected?.iconURL,
        mini: mini,
        message: message,
        text: sanitized,
        isBot: message.isBot,
        isLoading: isLoading,
        onConversationTap: (id) => widget.setConversationID?.call(id, true),
      ));
    }

    // Tool call badges
    if (!mini && message.hasToolCalls) {
      widgets.addAll(_buildToolCallWidgets(
        message, allMessagesForLookup, isProcessing,
        index, visibleMessageCount, mini,
        excludeIds: excludeToolIds,
      ));
    }

    return widgets;
  }

  /// Build the streaming section: renders items in arrival order, reusing
  /// _buildMessageWidgets so rendering stays in sync with the normal path.
  List<Widget> _buildStreamingWidgets(
    ChatSessionState sessionState,
    List<Message> allMessages,
    bool mini,
  ) {
    final widgets = <Widget>[];
    final isProcessing = sessionState.state == ConversationState.processing;

    for (final item in sessionState.items) {
      switch (item.type) {
        case 'text':
          if (item.text.isNotEmpty) {
            final syntheticMessage = Message(
              role: 'assistant',
              content: [ContentPart(type: 'text', text: item.text)],
            );
            widgets.addAll(_buildMessageWidgets(
              message: syntheticMessage,
              allMessagesForLookup: allMessages,
              isProcessing: isProcessing,
              index: 0,
              visibleMessageCount: 1,
              mini: mini,
              isLoading: item == sessionState.items.last && isProcessing,
            ));
          }
          break;

        case 'tool_use':
          if (!mini && item.toolCall != null) {
            final toolMessage = Message(
              role: 'assistant',
              content: const [],
              toolCalls: [item.toolCall!],
            );
            final resultItem = sessionState.items
                .where((i) => i.type == 'tool_result' && i.toolCallId == item.toolCall!.id)
                .firstOrNull;
            final lookupMessages = <Message>[
              if (resultItem != null)
                Message(
                  role: 'tool',
                  toolCallId: resultItem.toolCallId,
                  content: [ContentPart(type: 'text', text: resultItem.toolResult ?? '')],
                ),
            ];
            widgets.addAll(_buildMessageWidgets(
              message: toolMessage,
              allMessagesForLookup: lookupMessages,
              isProcessing: isProcessing,
              index: 0,
              visibleMessageCount: 1,
              mini: mini,
            ));
          }
          break;

        // tool_result items are rendered by the tool_use case above
      }
    }

    // Loading indicator if processing but nothing to show yet
    if (widgets.isEmpty && isProcessing) {
      widgets.add(const Padding(
        padding: EdgeInsets.all(8.0),
        child: SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2)),
      ));
    }

    return widgets;
  }

  /// Derive pending ask tools purely from conversation data.
  /// Looks for the last assistant message with tool calls that have no
  /// corresponding tool result, and cross-references with model.tools for "ask" mode.
  List<ToolCall> _derivePendingAskTools(List<Message> messages) {
    final toolsMap = _modelSelected.text?.tools ?? {};

    // Find tool call IDs that already have results
    final resolvedToolCallIds = messages
        .where((m) => m.role == 'tool' && m.toolCallId != null)
        .map((m) => m.toolCallId!)
        .toSet();

    // Find unresolved tool calls from assistant messages where mode is "ask"
    final pending = <ToolCall>[];
    for (final msg in messages.reversed) {
      if (msg.role == 'assistant' && msg.toolCalls != null) {
        for (final tc in msg.toolCalls!) {
          if (!resolvedToolCallIds.contains(tc.id) && toolsMap[tc.function.name] == 'ask') {
            pending.add(tc);
          }
        }
        break; // Only check the last assistant message with tool calls
      }
    }
    return pending;
  }

  Widget _buildApprovalBanner(List<ToolCall> askTools) {
    return ToolApprovalBanner(
      askTools: askTools,
      enrichToolCall: _enrichToolCall,
      onSubmit: (decisions) {
        _chatService.approveTools(
          conversationId: widget.conversationId,
          modelSelected: _modelSelected,
          askTools: askTools,
          decisions: decisions,
        );
      },
    );
  }

  /// Enrich a tool call with cached metadata if not already set.
  ToolCall _enrichToolCall(ToolCall tc) {
    if (tc.loading.isNotEmpty && tc.iconURL.isNotEmpty) return tc;
    final meta = _chatService.getToolMetadata(tc.function.name);
    if (meta == null) return tc;
    return ToolCall(
      id: tc.id, type: tc.type, function: tc.function,
      loading: tc.loading.isNotEmpty ? tc.loading : (meta['loading'] ?? ''),
      iconURL: tc.iconURL.isNotEmpty ? tc.iconURL : (meta['icon_url'] ?? ''),
    );
  }

  /// Parse a tool result string — may be plain text or JSON with images.
  ({String text, List<String> images}) _parseToolResult(String result) {
    try {
      if (result.startsWith('{')) {
        final parsed = jsonDecode(result);
        if (parsed is Map && parsed.containsKey('content') && parsed['content'] is List) {
          String text = '';
          List<String> images = [];
          for (final item in parsed['content']) {
            if (item['type'] == 'text') text = item['text'] ?? '';
            if (item['type'] == 'image' && item['data'] != null) images.add(item['data']);
          }
          return (text: text, images: images);
        }
      }
    } catch (_) {}
    return (text: result, images: <String>[]);
  }

  /// Build tool call badge widgets for a message, looking up results and metadata.
  /// [excludeIds] skips tool calls that are already rendered in the streaming section.
  List<Widget> _buildToolCallWidgets(
    Message message,
    List<Message> allMessagesForLookup,
    bool isProcessing,
    int index,
    int visibleMessageCount,
    bool mini, {
    Set<String> excludeIds = const {},
  }) {
    if (!message.hasToolCalls) return [];
    return message.toolCalls!
        .where((tc) => !excludeIds.contains(tc.id) && !isHiddenWaitResume(tc))
        .expand((toolCall) {
      final toolResultMessage = allMessagesForLookup
          .where((m) => m.role == 'tool' && m.toolCallId == toolCall.id)
          .firstOrNull;

      final resultContent = toolResultMessage?.textContent ?? '';
      final parsed = _parseToolResult(resultContent);
      final displayToolCall = _enrichToolCall(toolCall);

      final widgets = <Widget>[
        ToolCallBadge(
          toolCall: displayToolCall,
          isLoading: isProcessing && index >= visibleMessageCount - 1 && toolResultMessage == null,
          resultMessage: toolResultMessage,
        ),
      ];

      // read_attach renders its own attachment preview inside the badge —
      // skip the default image-preview pass to avoid duplicate display.
      final isReadAttach = isFsReadAttachCall(
        toolCall.function.name,
        toolCall.function.arguments,
      );

      if (toolCall.function.name != 'conversation_attachments' && !isReadAttach) {
        for (final url in parsed.images) {
          widgets.add(ImagePreviewComponent(imageUrl: url, mini: mini));
        }
        if (toolResultMessage != null && toolResultMessage.hasImages) {
          for (final url in toolResultMessage.imageUrls) {
            widgets.add(ImagePreviewComponent(imageUrl: url, mini: mini));
          }
        }
      }

      return widgets;
    }).toList();
  }

  // Function to build a message list
  Widget buildMessageList({
    required List<Message> messages,
    required ScrollController controller,
    bool mini = false,
    EdgeInsetsGeometry? padding,
  }) {
    final sessionState = _session.value;
    final isProcessing = sessionState.state == ConversationState.processing;
    final hasStreamingContent = sessionState.hasContent;

    // Derive pending ask tools from conversation data:
    // conversation is in waiting_for_tool state, last assistant message has tool calls
    // with no matching tool result — cross-reference with model.tools map for "ask" mode
    final pendingAskTools = isProcessing ? <ToolCall>[] : _derivePendingAskTools(messages);
    final hasPendingApproval = pendingAskTools.isNotEmpty;

    // Compute tool IDs already shown in streaming section
    final streamingToolIds = sessionState.items
        .where((i) => i.type == 'tool_use' && i.toolCall != null)
        .map((i) => i.toolCall!.id)
        .toSet();

    // Filter out tool result messages and dedupe against streaming.
    // Skip any DB message whose content is already rendered by the streaming section.
    var visibleMessages = messages.where((m) {
      if (m.role == 'tool') return false;
      // Synthetic "Timer is done" assistant messages exist only as a hook for
      // the LLM — they have no user-facing payload.
      if (isHiddenWaitResumeMessage(m)) return false;
      if (!hasStreamingContent) return true;
      // Skip assistant messages that are fully covered by streaming items
      if (m.role == 'assistant') {
        final hasText = m.textContent.isNotEmpty;
        final hasToolCalls = m.toolCalls != null && m.toolCalls!.isNotEmpty;
        final allToolsInStream = hasToolCalls &&
            m.toolCalls!.every((tc) => streamingToolIds.contains(tc.id));
        final textInStream = hasText && sessionState.streamingText.contains(m.textContent);
        // If everything in this message is already in streaming, skip it
        if ((!hasText || textInStream) && (!hasToolCalls || allToolsInStream)) return false;
      }
      return true;
    }).toList();

    final itemCount = visibleMessages.length
        + (hasStreamingContent ? 1 : 0)
        + (hasPendingApproval ? 1 : 0);
    if (!mini) _lastMainItemCount = itemCount;

    var l = SuperListView.builder(
      listController: mini ? _miniMapListController : _listController,
      controller: controller,
      cacheExtent: 100,
      padding: padding ?? const EdgeInsets.all(16.0),
      itemCount: itemCount,
      itemBuilder: (context, index) {
        // --- Streaming content ---
        // Rebuild ONLY this item per SSE token. Previously the whole list
        // (and the whole chat) rebuilt on every token via _onSessionChanged's
        // unconditional setState — every DB message's markdown re-rendered
        // per token (GPU stalls, jank). The slot below listens to _session
        // itself and re-renders just this item when the streamed text grows.
        if (hasStreamingContent && index == visibleMessages.length) {
          return Align(
            alignment: Alignment.centerLeft,
            child: ValueListenableBuilder<ChatSessionState>(
              valueListenable: _session,
              builder: (context, s, _) => Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: _buildStreamingWidgets(s, messages, mini),
              ),
            ),
          );
        }

        // --- Approval banner (shown whenever tools need approval) ---
        final approvalIndex = visibleMessages.length + (hasStreamingContent ? 1 : 0);
        if (hasPendingApproval && index == approvalIndex) {
          return _buildApprovalBanner(pendingAskTools);
        }

        // --- DB-loaded message ---
        final message = visibleMessages[index];
        final childWidgets = _buildMessageWidgets(
          message: message,
          allMessagesForLookup: messages,
          isProcessing: isProcessing,
          index: index,
          visibleMessageCount: visibleMessages.length,
          mini: mini,
          excludeToolIds: streamingToolIds,
        );

        return Align(
          alignment: message.isBot ? Alignment.centerLeft : Alignment.centerRight,
          child: Column(
            crossAxisAlignment: message.isBot ? CrossAxisAlignment.start : CrossAxisAlignment.end,
            children: childWidgets,
          ),
        );
      },
    );

    return ScrollConfiguration(
      behavior: ScrollConfiguration.of(context).copyWith(scrollbars: false),
      child:
          mini
              ? l
              : Scrollbar(
                thickness: widget.isMobile ? 8.0 : 14.0,
                radius:
                    widget.isMobile
                        ? Radius.circular(4.0)
                        : Radius.circular(0.0),
                controller: controller,
                thumbVisibility: widget.isMobile ? false : true,
                child: l,
              ),
    );
  }

  @override
  Widget build(BuildContext context) {
    final conversationsState = ref.watch(conversationsProvider);
    final globalStatus =
        _chatService.conversationStatuses.value[widget.conversationId];
    final isProcessing =
        _session.value.state == ConversationState.processing ||
        (globalStatus?.isProcessing ?? false);
    final streamingText = _session.value.streamingText;

    final currentConversation = conversationsState.conversations.firstWhere(
      (conv) => conv.id == widget.conversationId,
      orElse:
          () => Conversation(
            id: widget.conversationId,
            title: '',
            messages: [],
            lastMessageAt: DateTime.now(),
            modelSelected: _modelSelected,
          ),
    );

    // Sync the local cache from the live conversation so anything pushed
    // through Riverpod (the SSE handler mirroring server-stamped
    // model_selected on tool_use / done events, status-stream events,
    // or another part of the UI calling updateConversationMetaData) is
    // immediately visible — most importantly the client-lock badge and
    // the "locked on X" banner. _setSelectedModel pushes user edits into
    // Riverpod too, so this idempotent reassignment doesn't clobber any
    // in-flight picker change.
    if (widget.conversationId.isNotEmpty) {
      _modelSelected = currentConversation.modelSelected;
    }

    final messages = currentConversation.messages;

    // Dynamic bottom padding: give room for streaming response to grow
    // downward without pushing the user's just-sent message off-screen.
    final viewportHeight = MediaQuery.of(context).size.height;
    final dynamicBottomPadding = _needsBottomMargin ? (viewportHeight - 400) : 16.0;

    // One-shot: anchor to the newest item the first time an existing
    // conversation renders. Index-based jump is robust against unmeasured
    // items — the failure mode that broke the old animateTo(maxScrollExtent).
    if (!_didInitialScroll &&
        widget.conversationId.isNotEmpty &&
        messages.isNotEmpty) {
      _didInitialScroll = true;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) _scrollToBottom();
      });
    }

    // MiniMap is only shown on wide non-web desktop layouts. On web
    // (kIsWeb) it is never rendered — computing it doubles the per-build
    // message-list cost (every message's markdown parsed a second time per
    // frame, GPU/CanvasKit stalls). Declared before the content build below
    // so the minimap list is skipped entirely when hidden.
    final bool showMiniMap = !widget.isMobile && !kIsWeb && MediaQuery.of(context).size.width >= 1800;

    // Build main content and (only when visible) the minimap content
    final mainContent = buildMessageList(
      messages: messages,
      controller: _mainScrollController,
      padding: EdgeInsets.only(
        left: 16.0,
        right: 16.0,
        top: 16.0,
        bottom: dynamicBottomPadding,
      ),
    );

    final Widget? miniMapContent = showMiniMap
        ? buildMessageList(
            messages: messages,
            controller: _miniMapScrollController,
            mini: true,
            padding: EdgeInsets.only(
              left: 4.0,
              right: 4.0,
              top: 4.0,
              bottom: 0.0,
            ),
          )
        : null;

    final isDarkMode = Theme.of(context).brightness == Brightness.dark;

    // Combine both scrolling methods
    Widget chatContent = MiddleClickScroller(
      scrollController: _mainScrollController,
      iconColor: Theme.of(context).primaryColor,
      child: mainContent,
    );

    Widget newMessageScreen = Column(
      mainAxisAlignment: MainAxisAlignment.center,
      crossAxisAlignment: CrossAxisAlignment.center,
      children: [
        const SizedBox(height: 30),
        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          Center(
            child: Text(
              'Start a new conversation',
              style: Theme.of(context).textTheme.headlineSmall,
            ),
          ),

        if (_miniAppSelected != null && widget.conversationId.isEmpty)
          Center(
            child: Column(
              children: [
                SizedBox(height: 16),
                Container(
                  width: 250,
                  height: 250,
                  decoration: BoxDecoration(
                    borderRadius: BorderRadius.circular(1000.0),
                    boxShadow: [
                      BoxShadow(
                        color: Colors.black.withOpacity(0.1),
                        blurRadius: 23,
                        offset: const Offset(0, 2),
                      ),
                    ],
                  ),
                  clipBehavior: Clip.antiAlias,
                  child: Image.memory(
                    base64Decode(_miniAppSelected!.iconURL),
                    width: 250,
                    height: 250,
                    fit: BoxFit.cover,
                  ),
                ),
                SizedBox(height: 24),
                Container(
                  margin: const EdgeInsets.symmetric(
                    horizontal: 24.0,
                    vertical: 12.0,
                  ),
                  padding: const EdgeInsets.all(16.0),
                  decoration: BoxDecoration(
                    border: Border(
                      left: BorderSide(
                        color: Theme.of(context).colorScheme.primary,
                        width: 4.0,
                      ),
                    ),
                    color: Theme.of(
                      context,
                    ).colorScheme.surface.withOpacity(0.5),
                  ),
                  child: RichText(
                    text: TextSpan(
                      style: Theme.of(context).textTheme.headlineSmall,
                      children: [
                        TextSpan(
                          text: '"',
                          style: TextStyle(
                            fontSize: 20,
                            color: Theme.of(context).colorScheme.primary,
                            fontWeight: FontWeight.bold,
                          ),
                        ),
                        TextSpan(
                          style: Theme.of(context).textTheme.headlineSmall
                              ?.copyWith(fontSize: 20)
                              ?.copyWith(fontStyle: FontStyle.italic),
                          text:
                              _miniAppSelected!.initialMessage?['en'] ??
                              'What can I help you with?',
                        ),
                        TextSpan(
                          text: '"',
                          style: TextStyle(
                            fontSize: 20,
                            color: Theme.of(context).colorScheme.primary,
                            fontWeight: FontWeight.bold,
                          ),
                        ),
                      ],
                    ),
                  ),
                ),

                SizedBox(height: 24),
              ],
            ),
          ),

        if (_miniAppSelected != null && widget.conversationId.isEmpty)
          Container(
            child: MiniAppForm(
              app: _miniAppSelected!,
              onChanged: (message) {
                setState(() {
                  _miniAppPrePrompt = message;
                });
              },
            ),
          ),

        if (_miniAppSelected != null && widget.conversationId.isEmpty)
          SizedBox(height: 24),

        if (widget.conversationId.isNotEmpty) Expanded(child: chatContent),

        // Input Container
        Container(
          margin: const EdgeInsets.symmetric(horizontal: 16.0),
          padding:
              widget.conversationId.isNotEmpty
                  ? const EdgeInsets.symmetric(horizontal: 12.0, vertical: 8.0)
                  : const EdgeInsets.symmetric(
                    horizontal: 32.0,
                    vertical: 12.0,
                  ),
          decoration:
              widget.conversationId.isNotEmpty
                  ? BoxDecoration(
                    boxShadow: [
                      BoxShadow(
                        color: Colors.black.withOpacity(0.1),
                        blurRadius: 4,
                        offset: const Offset(0, 2),
                      ),
                    ],
                    color:
                        isDarkMode
                            ? Color.fromARGB(15, 255, 255, 255)
                            : Colors.white,
                    borderRadius: BorderRadius.only(
                      topLeft: Radius.circular(8.0),
                      topRight: Radius.circular(8.0),
                    ),
                    border: Border(
                      top: BorderSide(
                        color: isDarkMode ? Color(0x010101) : Color(0xFFEEEEEE),
                      ),
                      left: BorderSide(
                        color: isDarkMode ? Color(0x010101) : Color(0xFFEEEEEE),
                      ),
                      right: BorderSide(
                        color: isDarkMode ? Color(0x010101) : Color(0xFFEEEEEE),
                      ),
                    ),
                  )
                  : null,
          child: Form(
            key: _formKey,
            child: InputBox(
              isMobile: widget.isMobile,
              messageController: _messageController,
              addAttachment: _addAttachment,
              attachFile: _attachFile,
              onSend: _handleSubmit,
              isLoading: isProcessing,
              handleStop: _handleStop,
              pickImage: _pickImage,
              pickFile: _pickFile,
              inputFocusNode: _inputFocusNode,
              removeAttachment: _removeAttachment,
              setSelectedModel: _setSelectedModel,
              selectedModel: _modelSelected,
              allowEmptyMessage: _miniAppPrePrompt != "",
              attachments: attachments,
              conversationId: widget.conversationId,
              submitButton:
                  _miniAppSelected != null && widget.conversationId.isEmpty,
              placeholder:
                  (_miniAppSelected != null &&
                          widget.conversationId.isEmpty &&
                          _miniAppSelected!.placeholder != "")
                      ? _miniAppSelected!.placeholder
                      : 'Your message...',
            ),
          ),
        ),

        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          Row(
            mainAxisAlignment: MainAxisAlignment.center,
            crossAxisAlignment: CrossAxisAlignment.center,
            children: [
              Container(
                margin: const EdgeInsets.only(left: 16.0),
                child: IconButton(
                  onPressed:
                      () => launchUrlString('https://discord.gg/PwMWwsrwHA'),
                  icon: Icon(Icons.discord),
                ),
              ),
              Container(
                margin: const EdgeInsets.only(left: 16.0),
                child: IconButton(
                  onPressed:
                      () => launchUrlString(
                        'https://www.reddit.com/r/PluralityAI/',
                      ),
                  icon: Icon(Icons.reddit),
                ),
              ),
              Container(
                margin: const EdgeInsets.only(left: 16.0),
                child: IconButton(
                  onPressed:
                      () => launchUrlString('https://x.com/the_only_yann'),
                  icon: const Text(
                    '𝕏',
                    style: TextStyle(
                      fontSize: 20,
                      fontWeight: FontWeight.bold,
                    ),
                  ),
                ),
              ),
              SizedBox(width: 16),
            ],
          ),

        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          SizedBox(height: 32),

        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          MiniAppsBrowser(
            isMobile: widget.isMobile,
            showPinnedOnly: true,
            onStartMiniApp: (miniapp) {
              setState(() {
                _miniAppSelected = miniapp;

                if (miniapp.modelSelected != null) {
                  _modelSelected = ModelSelected(
                    text: miniapp.modelSelected!.text ?? _modelSelected.text,
                    vision:
                        miniapp.modelSelected!.vision ?? _modelSelected.vision,
                    imageGen:
                        miniapp.modelSelected!.imageGen ??
                        _modelSelected.imageGen,
                    imageEdit:
                        miniapp.modelSelected!.imageEdit ??
                        _modelSelected.imageEdit,
                  );
                }
              });
            },
            onEditMiniApp: (miniapp) => _editMiniApp(miniapp),
          ),
      ],
    );

    newMessageScreen = Center(
      child: ConstrainedBox(
        constraints: BoxConstraints(maxWidth: 1000),
        child: newMessageScreen,
      ),
    );

    return Stack(
      children: [
        widget.conversationId.isEmpty && _miniAppSelected != null
            ? Positioned.fill(
              child: Center(
                child: SingleChildScrollView(child: newMessageScreen),
              ),
            )
            : newMessageScreen,

        // MiniMap on the right edge of the screen
        if (showMiniMap && widget.conversationId.isNotEmpty)
          Positioned(
            top: 0,
            bottom: 0,
            right: 0,
            width: 140,
            child: MiniMap(
              enabled: true,
              mainScrollController: _mainScrollController,
              miniMapScrollController: _miniMapScrollController,
              miniMapContent: miniMapContent ?? const SizedBox.shrink(),
              overlayColor:
                  Theme.of(context).brightness == Brightness.dark
                      ? Theme.of(context).textTheme.bodySmall?.color ?? Colors.red
                      : Theme.of(context).primaryColor,
              miniMapWidth: 140,
              overlayHeight: 80,
              child: const SizedBox.shrink(),
            ),
          ),

        // Long-conversation warning: token usage grows as context grows.
        if (!_closeMessageWarning &&
            messages.isNotEmpty &&
            messages.last.totalTokens != null &&
            messages.last.totalTokens! > 100000 &&
            messages.length > 5)
          Container(
            width: double.infinity,
            margin: EdgeInsets.symmetric(horizontal: 12, vertical: 6),
            padding: EdgeInsets.symmetric(horizontal: 16, vertical: 8),
            decoration: BoxDecoration(
              color: Colors.orange,
              borderRadius: BorderRadius.circular(5),
            ),
            child: Row(
              children: [
                Icon(Icons.warning, color: Colors.white),
                SizedBox(width: 8),
                Expanded(
                  child: Text(
                    "Long conversations use more tokens per reply. Consider starting a new conversation when possible. Last message used ${messages.last.totalTokens} tokens (token use grows as the conversation gets longer).",
                    style: TextStyle(color: Colors.white, fontSize: 16),
                  ),
                ),
                IconButton(
                  icon: Icon(Icons.close, color: Colors.white),
                  onPressed: () {
                    setState(() {
                      _closeMessageWarning = true;
                    });
                  },
                ),
              ],
            ),
          ),

        // Client-lock banner: shown on every client EXCEPT the one that
        // currently holds the lock. The lock holder runs the client-side
        // tools (filesystem / shell / MCP); other clients still see the
        // message stream but skip tool dispatch. "Move conversation here"
        // claims the lock on this device — disabled while any client is
        // mid-step so we don't yank ownership during a tool execution.
        // Only mounted for real conversations: on the new-conversation screen
        // with a preset selected, every other Stack child is Positioned, so a
        // non-positioned SizedBox.shrink would size the whole Stack to 0x0
        // and blank the page.
        if (widget.conversationId.isNotEmpty)
          Builder(builder: (context) {
            final lock = currentConversation.modelSelected.clientLock;
            final myId = ClientIdentity().id;
            if (lock == null || myId.isEmpty || lock.id == myId) {
              return const SizedBox.shrink();
            }
            final busy = isProcessing ||
                _session.value.state == ConversationState.waitingForTool;
            final onBg = Theme.of(context).colorScheme.onSecondaryContainer;
            return Container(
              margin: EdgeInsets.symmetric(horizontal: 12, vertical: 4),
              padding: EdgeInsets.symmetric(horizontal: 10, vertical: 4),
              decoration: BoxDecoration(
                color: Theme.of(context).colorScheme.secondaryContainer,
                borderRadius: BorderRadius.circular(5),
                border: Border.all(
                  color: Theme.of(context).colorScheme.outline,
                ),
              ),
              child: Row(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Icon(Icons.lock_outline, size: 14, color: onBg),
                  SizedBox(width: 6),
                  Flexible(
                    child: Tooltip(
                      message:
                          "Client tools (files, shell) only run on '${lock.label}'.",
                      child: Text(
                        "Locked on '${lock.label}'",
                        style: TextStyle(color: onBg, fontSize: 12),
                        overflow: TextOverflow.ellipsis,
                        maxLines: 1,
                      ),
                    ),
                  ),
                  SizedBox(width: 4),
                  Tooltip(
                    message: busy
                        ? 'Wait for the current step to finish'
                        : 'Take ownership on this device',
                    child: TextButton(
                      style: TextButton.styleFrom(
                        padding: EdgeInsets.symmetric(horizontal: 8),
                        minimumSize: const Size(0, 28),
                        tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                        visualDensity: VisualDensity.compact,
                      ),
                      onPressed: busy
                          ? null
                          : () {
                              final claim = ClientIdentity().asLock();
                              if (claim == null) return;
                              // Drop the previous holder's folder attachment
                              // along with the lock claim — the path lives on
                              // the OTHER machine's filesystem.
                              ref
                                  .read(conversationsProvider.notifier)
                                  .updateConversationMetaData(
                                    conversationId: widget.conversationId,
                                    modelSelected: currentConversation
                                        .modelSelected
                                        .copyWith(
                                          clientLock: claim,
                                          clientFolderPath: null,
                                        ),
                                  );
                            },
                      child: const Text('Move here', style: TextStyle(fontSize: 12)),
                    ),
                  ),
                ],
              ),
            );
          }),

        if (widget.conversationId.isEmpty && _miniAppSelected != null)
          Positioned(
            left: 16,
            top: 16,
            child: IconButton(
              onPressed: () {
                setState(() {
                  _miniAppSelected = null;
                  _updateSelectedModel();
                });
              },
              iconSize: 32,
              icon: Icon(Icons.close),
            ),
          ),

        if (!_isNearBottom)
          Positioned(
            right: 16,
            bottom: 80,
            child: FloatingActionButton(
              mini: true,
              onPressed: _scrollToBottom,
              child: const Icon(Icons.arrow_downward),
            ),
          ),
      ],
    );
  }
}

/// Self-contained approval banner widget with its own local state.
/// State is destroyed when the widget unmounts (conversation changes, approval submitted).
class ToolApprovalBanner extends StatefulWidget {
  final List<ToolCall> askTools;
  final ToolCall Function(ToolCall) enrichToolCall;
  final void Function(Map<String, bool> decisions) onSubmit;

  const ToolApprovalBanner({
    super.key,
    required this.askTools,
    required this.enrichToolCall,
    required this.onSubmit,
  });

  @override
  State<ToolApprovalBanner> createState() => _ToolApprovalBannerState();
}

class _ToolApprovalBannerState extends State<ToolApprovalBanner> {
  final Map<String, bool> _decisions = {};

  /// Per-tool scroll controllers so each args strip has its own Scrollbar.
  /// Lazily created in [_argsScrollController] and disposed on unmount.
  final Map<String, ScrollController> _argsControllers = {};

  ScrollController _argsScrollController(String toolCallId) {
    return _argsControllers.putIfAbsent(toolCallId, () => ScrollController());
  }

  @override
  void dispose() {
    for (final c in _argsControllers.values) {
      c.dispose();
    }
    super.dispose();
  }

  String _formatArgs(String argsJson) {
    try {
      final args = Map<String, dynamic>.from(jsonDecode(argsJson));
      if (args.isEmpty) return '';
      return args.entries.map((e) => '${e.key}: ${e.value}').join('\n');
    } catch (_) {
      return argsJson;
    }
  }

  String _displayLabel(ToolCall tc) {
    final enriched = widget.enrichToolCall(tc);
    String label = enriched.loading.isNotEmpty ? enriched.loading : formatToolDisplayName(enriched.function.name);
    try {
      final args = Map<String, dynamic>.from(jsonDecode(tc.function.arguments));
      args.forEach((key, value) {
        label = label.replaceAll('{{$key}}', value.toString());
      });
    } catch (_) {}
    return label.replaceAll(RegExp(r'\{\{.*?\}\}'), '').replaceAll('  ', ' ').trim();
  }

  void _decide(String toolCallId, bool approved) {
    setState(() => _decisions[toolCallId] = approved);
    if (widget.askTools.every((tc) => _decisions.containsKey(tc.id))) {
      widget.onSubmit(Map<String, bool>.from(_decisions));
    }
  }

  void _decideAll(bool approved) {
    for (final tc in widget.askTools) {
      _decisions[tc.id] = approved;
    }
    widget.onSubmit(Map<String, bool>.from(_decisions));
  }

  @override
  Widget build(BuildContext context) {
    final isDark = Theme.of(context).brightness == Brightness.dark;
    final accentColor = Theme.of(context).colorScheme.primary;

    return FractionallySizedBox(
      widthFactor: 0.5,
      alignment: Alignment.centerLeft,
      child: Container(
        margin: const EdgeInsets.symmetric(vertical: 8),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(12),
          border: Border.all(color: accentColor.withOpacity(0.4)),
          color: accentColor.withOpacity(isDark ? 0.08 : 0.04),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 12, 16, 8),
              child: Row(
                children: [
                  Icon(Icons.shield_outlined, size: 18, color: accentColor),
                  const SizedBox(width: 8),
                  Text(
                    'Approval required',
                    style: TextStyle(
                      fontWeight: FontWeight.w600,
                      fontSize: 14,
                      color: Theme.of(context).colorScheme.onSurface,
                    ),
                  ),
                ],
              ),
            ),
            // Tool cards
            ...widget.askTools.map((tc) {
              final label = _displayLabel(tc);
              final argsFormatted = _formatArgs(tc.function.arguments);
              final enriched = widget.enrichToolCall(tc);
              final decision = _decisions[tc.id];

              return Padding(
                padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 4),
                child: Container(
                  width: double.infinity,
                  padding: const EdgeInsets.all(10),
                  decoration: BoxDecoration(
                    color: decision == null
                        ? Theme.of(context).colorScheme.surface
                        : decision
                            ? Colors.green.withOpacity(isDark ? 0.1 : 0.05)
                            : Colors.red.withOpacity(isDark ? 0.1 : 0.05),
                    borderRadius: BorderRadius.circular(8),
                    border: Border.all(
                      color: decision == null
                          ? Theme.of(context).colorScheme.outline.withOpacity(0.2)
                          : decision
                              ? Colors.green.withOpacity(0.4)
                              : Colors.red.withOpacity(0.4),
                    ),
                  ),
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Row(
                        children: [
                          if (enriched.iconURL.isNotEmpty)
                            Padding(
                              padding: const EdgeInsets.only(right: 8),
                              child: SizedBox(
                                width: 16, height: 16,
                                child: Image.memory(
                                  base64Decode(enriched.iconURL),
                                  width: 16, fit: BoxFit.cover, cacheWidth: 16,
                                  gaplessPlayback: true,
                                ),
                              ),
                            )
                          else
                            Padding(
                              padding: const EdgeInsets.only(right: 8),
                              child: Icon(Icons.extension, size: 16,
                                color: Theme.of(context).colorScheme.onSurfaceVariant),
                            ),
                          Expanded(
                            child: Text(
                              label,
                              style: TextStyle(
                                fontWeight: FontWeight.w500,
                                fontSize: 13,
                                color: Theme.of(context).colorScheme.onSurface,
                              ),
                            ),
                          ),
                          if (decision == null) ...[
                            OutlinedButton(
                              onPressed: () => _decide(tc.id, false),
                              style: OutlinedButton.styleFrom(
                                foregroundColor: Theme.of(context).colorScheme.error,
                                side: BorderSide(color: Theme.of(context).colorScheme.error.withOpacity(0.5)),
                                padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                                minimumSize: const Size(0, 36),
                              ),
                              child: const Text('Deny'),
                            ),
                            const SizedBox(width: 6),
                            FilledButton(
                              onPressed: () => _decide(tc.id, true),
                              style: FilledButton.styleFrom(
                                backgroundColor: Colors.green,
                                padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                                minimumSize: const Size(0, 36),
                              ),
                              child: const Text('Approve'),
                            ),
                          ] else
                            Icon(
                              decision ? Icons.check_circle : Icons.cancel,
                              size: 20,
                              color: decision ? Colors.green : Theme.of(context).colorScheme.error,
                            ),
                        ],
                      ),
                      if (() {
                        final diff = buildFsWriteDiff(
                          toolName: tc.function.name,
                          argumentsJson: tc.function.arguments,
                          context: context,
                          maxHeight: 220,
                        );
                        return diff != null;
                      }()) ...[
                        const SizedBox(height: 6),
                        Builder(builder: (context) {
                          final diff = buildFsWriteDiff(
                            toolName: tc.function.name,
                            argumentsJson: tc.function.arguments,
                            context: context,
                            maxHeight: 220,
                          )!;
                          return diff;
                        }),
                      ] else if (argsFormatted.isNotEmpty) ...[
                        const SizedBox(height: 6),
                        ConstrainedBox(
                          constraints: const BoxConstraints(maxHeight: 70),
                          child: Scrollbar(
                            controller: _argsScrollController(tc.id),
                            thumbVisibility: true,
                            thickness: 4,
                            radius: const Radius.circular(2),
                            child: SingleChildScrollView(
                              controller: _argsScrollController(tc.id),
                              child: Container(
                                width: double.infinity,
                                padding: const EdgeInsets.all(8),
                                decoration: BoxDecoration(
                                  color: Theme.of(context).colorScheme.surfaceContainerHighest.withOpacity(0.5),
                                  borderRadius: BorderRadius.circular(6),
                                ),
                                child: Text(
                                  argsFormatted,
                                  style: TextStyle(
                                    fontSize: 12,
                                    fontFamily: 'monospace',
                                    color: Theme.of(context).colorScheme.onSurfaceVariant,
                                  ),
                                ),
                              ),
                            ),
                          ),
                        ),
                      ],
                    ],
                  ),
                ),
              );
            }),
            // Bulk buttons (only for multiple tools)
            if (widget.askTools.length > 1)
              Padding(
                padding: const EdgeInsets.fromLTRB(12, 8, 12, 12),
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.end,
                  children: [
                    TextButton(
                      onPressed: () => _decideAll(false),
                      child: const Text('Deny All'),
                    ),
                    TextButton(
                      onPressed: () => _decideAll(true),
                      child: const Text('Approve All'),
                    ),
                  ],
                ),
              ),
          ],
        ),
      ),
    );
  }
}