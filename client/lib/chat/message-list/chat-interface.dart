import 'package:flutter/foundation.dart'
    show ValueNotifier, kIsWeb;
import 'package:plurality/chat/message-list/attachments.dart';
import 'package:url_launcher/url_launcher_string.dart';
import '../snackbar.dart';
import 'package:flutter/material.dart';
import 'dart:convert';
import '../../utils/types.dart';
import '../../api/api.dart';
import '../../api/service.dart';
import '../../api/chat_service.dart';
import '../../api/tts.dart';
import '../../api/mini-apps.dart';
import '../../api/balance.dart';
import '../../api/preferences_provider.dart';
import 'AnimatedMessageBox.dart';
import 'package:image_picker/image_picker.dart';
import 'package:file_picker/file_picker.dart';
import 'package:top_snackbar_flutter/top_snack_bar.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'image.dart';
import 'input.dart';
import 'model-picker.dart';
import '../../utils/index.dart';
import '../../utils/file-types.dart';
import '../../api/stt.dart';
import 'minimap.dart';
import '../miniapps.dart';
import './MiniAppForm.dart';
import 'middle-click.dart';
import 'package:super_sliver_list/super_sliver_list.dart';
import 'toolcall-badge.dart';

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

class _ChatInterfaceState extends ConsumerState<ChatInterface> {
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
  bool _hasScrolledToUserMessage = false;
  bool _needsBottomMargin = false;
  bool _closeMessageWarning = false;

  ModelSelected _modelSelected = ModelSelected();

  MiniApp? _miniAppSelected;

  String _miniAppPrePrompt = '';

  /// Observable session state from ChatService.
  late ValueNotifier<ChatSessionState> _session;

  @override
  void initState() {
    super.initState();
    _mainScrollController.addListener(_scrollListener);

    _miniAppPrePrompt = '';

    // Initialize the session notifier from ChatService
    _session = _chatService.getSession(widget.conversationId);
    _session.addListener(_onSessionChanged);

    final preferencesNotifier = ref.read(preferencesProvider.notifier);
    preferencesNotifier.loadAllPreferences().then((_) {
      _updateSelectedModel();
    });

    _updateSelectedModel();
    _loadConversation(widget.conversationId);

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

    _miniAppPrePrompt = '';

    // Check if the conversationId has changed
    if (oldWidget.conversationId != widget.conversationId) {
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

      setState(() {
        _hasScrolledToUserMessage = false;
        _needsBottomMargin = false;
        _closeMessageWarning = false;
      });
    }
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

    // One-time: when streaming starts, scroll user message to top and enable bottom margin
    if (state.state == ConversationState.processing && !_hasScrolledToUserMessage) {
      _hasScrolledToUserMessage = true;
      _needsBottomMargin = true;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (!mounted || !_listController.isAttached || !_mainScrollController.hasClients) return;

        final conversationsState = ref.read(conversationsProvider);
        final currentConversation = conversationsState.conversations.firstWhere(
          (conv) => conv.id == widget.conversationId,
          orElse: () => Conversation(
            id: widget.conversationId, title: '', messages: [],
            lastMessageAt: DateTime.now(), modelSelected: _modelSelected,
          ),
        );
        final visibleMessages = currentConversation.messages.where((m) => m.role != 'tool').toList();
        final lastUserIndex = visibleMessages.lastIndexWhere((m) => m.role == 'user');
        if (lastUserIndex < 0) return;

        _listController.jumpToItem(
          index: lastUserIndex,
          scrollController: _mainScrollController,
          alignment: 0.0,
        );
      });
    }

    // Reset when streaming ends and focus the input
    if (state.state != ConversationState.processing) {
      _hasScrolledToUserMessage = false;
      Future.delayed(Duration(milliseconds: 150), () {
        if (mounted) _inputFocusNode.requestFocus();
      });
    }

    setState(() {});
  }

  // Extract the model selection logic to a separate method
  void _updateSelectedModel() {
    final balanceState = ref.read(balanceProvider);
    final balance = balanceState.value;
    final isFree = balance?.planName == 'Free';

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
      // Fall back to the globally selected model from preferences
      _modelSelected = preferences.selectedModel;
      _miniAppSelected = null;

      // if free plan, set model to free plan
      if (isFree) {
        _modelSelected = ModelSelectionModalState.getFastPreset();
      }
    }
  }

  // Extract the model selection logic to a separate method
  void _loadConversation(String id) {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    try {
      conversationsNotifier.loadConversation(id);
    } catch (e) {
      // if APINeedEmailVerify
      // show email verify page

      if (e.toString().contains('APINeedEmailVerify')) {
        Navigator.of(context).pushNamed('/verify-email');
      } else {
        SnackBar(
          content: Text('Failed to load conversation: $e'),
          showCloseIcon: true,
        );
      }
    }
  }

  void _setSelectedModel(ModelSelected modelSelected) {
    setState(() {
      _modelSelected = modelSelected;
    });

    if (widget.conversationId.isEmpty) {
      final preferencesNotifier = ref.read(preferencesProvider.notifier);
      preferencesNotifier.setSelectedModel(modelSelected);
    }
  }

  @override
  void dispose() {
    _session.removeListener(_onSessionChanged);
    _listController.dispose();
    _miniMapListController.dispose();
    _mainScrollController.dispose();
    _miniMapScrollController.dispose();
    _inputFocusNode.dispose();
    // Guard: controller may already be disposed if widget was torn down during navigation
    try { _messageController.dispose(); } catch (_) {}
    super.dispose();
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
        _mainScrollController.position.pixels >=
        _contentMaxExtent - 200;
    if (isNearBottom != _isNearBottom) {
      setState(() {
        _isNearBottom = isNearBottom;
      });
    }
  }

  void _handleStop() {
    _chatService.cancel(widget.conversationId);
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
        final PlatformFile file = result.files.single;
        final xFile = file.xFile;
        final mimeType = file.extension ?? 'binary/octet-stream';
        // if is in textFileExtensions see as snippet

        if (textFileExtensions.contains(mimeType)) {
          final bytes = await xFile.readAsBytes();
          final text = utf8.decode(bytes);
          setState(() {
            attachments.add(
              Attachment(
                type: 'snippet',
                filename: file.name,
                ext: mimeType,
                content: text,
              ),
            );
          });
        } else {
          final bytes = await xFile.readAsBytes();
          final base64Data = base64Encode(bytes);
          final attType = mimeType == 'pdf' ? 'pdf' : 'file';

          setState(() {
            attachments.add(
              Attachment(
                type: attType,
                filename: file.name,
                ext: mimeType,
                content: 'data:$mimeType;base64,$base64Data',
              ),
            );
          });
        }
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

  Future<void> _handleSubmit(BuildContext context) async {
    if (_formKey.currentState!.validate()) {
      final userMessage = _miniAppPrePrompt + _messageController.text;
      _messageController.clear();
      _miniAppPrePrompt = '';
      await _submitMessage(userMessage);
    }
  }

  /// Create a Message from user input and delegate to ChatService.
  Future<void> _submitMessage(String? userMessage) async {
    _needsBottomMargin = false;
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    final balanceNotifier = ref.read(balanceProvider.notifier);

    // Build the user message
    Message? newMessage;
    if ((userMessage != null && userMessage.isNotEmpty) ||
        attachments.isNotEmpty) {
      // Check if there are image attachments
      final imageAttachments =
          attachments.where((a) => a.type == 'image_url').toList();
      final pdfAttachments =
          attachments.where((a) => a.type == 'pdf').toList();
      final otherAttachments =
          attachments.where((a) => a.type != 'image_url' && a.type != 'pdf').toList();

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

      // Add PDF attachments
      for (final att in pdfAttachments) {
        contentParts.add(ContentPart(type: 'pdf', text: att.content, filename: att.filename));
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

    if (newMessage == null) return;

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
        return;
      }

      if (!mounted) return;
      balanceNotifier.refresh();
      Future.delayed(Duration(milliseconds: 150), () {
        if (mounted) _inputFocusNode.requestFocus();
      });
    } catch (e, s) {
      print('Error: $e');
      print('Stack: $s');
      if (context.mounted) {
        showTopSnackBar(
          Overlay.of(context),
          PrettySnackbar(message: e.toString(), type: SnackbarType.error),
        );
      }
    }
  }

  /// Build the streaming section: renders items in arrival order, reusing
  /// _buildToolCallWidgets so tool rendering stays in sync with the normal path.
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
            widgets.add(AnimatedMessageBox(
              iconURL: _miniAppSelected?.iconURL,
              mini: mini,
              message: Message(role: 'assistant', content: [ContentPart(type: 'text', text: item.text)]),
              text: sanitizeMessages(item.text),
              isBot: true,
              isLoading: item == sessionState.items.last && isProcessing,
            ));
          }
          break;

        case 'tool_use':
          if (!mini && item.toolCall != null) {
            // Wrap in a temporary Message and delegate to _buildToolCallWidgets
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
            widgets.addAll(_buildToolCallWidgets(
              toolMessage, lookupMessages, isProcessing, 0, 1, mini,
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
        .where((tc) => !excludeIds.contains(tc.id))
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
          result: toolResultMessage != null ? ContentPart(type: 'text', text: parsed.text) : null,
        ),
      ];

      if (toolCall.function.name != 'conversation_attachments') {
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

    // Filter out tool result messages — they're rendered inline with tool call badges.
    // When streaming, also skip assistant/tool messages added by the server mid-loop
    // (after the last user message) — the streaming section renders those.
    var visibleMessages = messages.where((m) => m.role != 'tool').toList();
    if (hasStreamingContent) {
      final lastUserIndex = visibleMessages.lastIndexWhere((m) => m.role == 'user');
      if (lastUserIndex >= 0) {
        visibleMessages = visibleMessages.sublist(0, lastUserIndex + 1);
      }
    }

    var l = SuperListView.builder(
      listController: mini ? _miniMapListController : _listController,
      controller: controller,
      cacheExtent: 100,
      padding: padding ?? const EdgeInsets.all(16.0),
      itemCount: visibleMessages.length + (hasStreamingContent ? 1 : 0),
      itemBuilder: (context, index) {
        // --- Streaming item: render SSE events in order ---
        if (index >= visibleMessages.length) {
          return Align(
            alignment: Alignment.centerLeft,
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: _buildStreamingWidgets(sessionState, messages, mini),
            ),
          );
        }

        // --- DB-loaded message ---
        final message = visibleMessages[index];
        final messageText = message.textContent;

        var textWidget =
            sanitizeMessages(messageText) != ""
                ? AnimatedMessageBox(
                  iconURL: _miniAppSelected?.iconURL,
                  mini: mini,
                  message: message,
                  text: sanitizeMessages(messageText),
                  isBot: message.isBot,
                  isLoading: false,
                )
                : null;

        return Align(
          alignment: message.isBot ? Alignment.centerLeft : Alignment.centerRight,
          child: Column(
            crossAxisAlignment: message.isBot ? CrossAxisAlignment.start : CrossAxisAlignment.end,
            children: [
              // Images and other non-text content
              ...message.content.where((c) => c.type != 'text').map(
                (part) => AttachmentViewer(
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
                ),
              ),
              if (textWidget != null) textWidget,
              // Tool call badges — dedupe against streaming items
              if (!mini && message.hasToolCalls)
                ..._buildToolCallWidgets(
                  message, messages, isProcessing, index, visibleMessages.length, mini,
                  excludeIds: sessionState.items
                      .where((i) => i.type == 'tool_use' && i.toolCall != null)
                      .map((i) => i.toolCall!.id)
                      .toSet(),
                ),
            ],
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
    final isProcessing =
        _session.value.state == ConversationState.processing;
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

    final messages = currentConversation.messages;

    // Dynamic bottom padding: give room for streaming response to grow
    final viewportHeight = MediaQuery.of(context).size.height;
    final dynamicBottomPadding = _needsBottomMargin ? (viewportHeight - 400) : 16.0;

    // Build main content and minimap content using the shared function
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

    final miniMapContent = buildMessageList(
      messages: messages,
      controller: _miniMapScrollController,
      mini: true,
      padding: EdgeInsets.only(
        left: 4.0,
        right: 4.0,
        top: 4.0,
        bottom: 0.0,
      ),
    );

    final isDarkMode = Theme.of(context).brightness == Brightness.dark;

    final bool showMiniMap = !widget.isMobile && !kIsWeb && MediaQuery.of(context).size.width >= 1800;

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
                      () => launchUrlString('https://discord.gg/qHMS7rvNjE'),
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
              SizedBox(width: 16),
            ],
          ),

        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          SizedBox(height: 32),

        if (widget.conversationId.isEmpty && _miniAppSelected == null)
          MiniAppsBrowser(
            isMobile: widget.isMobile,
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
                  );
                }
              });
            },
          ),
      ],
    );

    newMessageScreen = Center(
      child: ConstrainedBox(
        constraints: BoxConstraints(maxWidth: 1200),
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
              miniMapContent: miniMapContent,
              overlayColor:
                  Theme.of(context).brightness == Brightness.dark
                      ? Theme.of(context).textTheme.bodySmall?.color ?? Colors.red
                      : Theme.of(context).primaryColor,
              miniMapWidth: 140,
              overlayHeight: 80,
              child: const SizedBox.shrink(),
            ),
          ),

        // if last message of messages has message.TokenPrice > 1000
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
                    "Replying to long conversation costs more credits. Consider starting a new conversation whenever possible. Last message cost was " +
                        messages.last.totalTokens.toString() +
                        " credits (message cost goes up and down as the conversation goes on).",
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

        // Scroll to bottom button
        if (!_isNearBottom)
          Positioned(
            right: 16,
            bottom: 80,
            child: FloatingActionButton(
              mini: true,
              onPressed: () => _mainScrollController.animateTo(
                _contentMaxExtent,
                duration: Duration(milliseconds: 300),
                curve: Curves.easeOut,
              ),
              child: const Icon(Icons.arrow_downward),
            ),
          ),
      ],
    );
  }
}
