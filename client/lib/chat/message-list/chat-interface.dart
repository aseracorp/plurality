import 'package:flutter/foundation.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/chat/message-list/attachments.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:url_launcher/url_launcher_string.dart';
import '../snackbar.dart';
import 'package:flutter/material.dart';
import 'package:flutter/foundation.dart';
import 'dart:convert';
import '../../utils/types.dart';
import '../../api/api.dart';
import '../../api/service.dart';
import '../../api/tts.dart';
import '../../api/mini-apps.dart';
import '../../api/balance.dart';
import '../../api/preferences_provider.dart';
import 'package:flutter/services.dart';
import 'AnimatedMessageBox.dart';
import 'package:image_picker/image_picker.dart';
import 'package:file_picker/file_picker.dart';
import 'package:top_snackbar_flutter/top_snack_bar.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'image.dart';
import 'image-gen.dart';
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
import 'package:collection/collection.dart';
import 'package:flutter/foundation.dart'
    show defaultTargetPlatform, kIsWeb, TargetPlatform;

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
  final FocusNode _inputFocusNode = FocusNode();
  final GlobalKey<FormState> _formKey = GlobalKey<FormState>();
  final TextEditingController _messageController = TextEditingController();

  // Main content scroll controller
  final ScrollController _mainScrollController = ScrollController();

  // MiniMap scroll controller
  final ScrollController _miniMapScrollController = ScrollController();

  final ImagePicker _imagePicker = ImagePicker();

  final List<Attachment> attachments = [];

  String _currentStreamedResponse = '';
  bool _isLoading = false;
  bool _shouldAutoScroll = true;
  bool _interrupted = false;
  bool _closeMessageWarning = false;

  ModelSelected _modelSelected = ModelSelected();

  MiniApp? _miniAppSelected;

  String _miniAppPrePrompt = '';

  @override
  void initState() {
    super.initState();
    _mainScrollController.addListener(_scrollListener);

    _miniAppPrePrompt = '';

    final preferencesNotifier = ref.read(preferencesProvider.notifier);
    preferencesNotifier.loadAllPreferences().then((_) {
      _updateSelectedModel();
    });

    _updateSelectedModel();
    _loadConversation(widget.conversationId);

    // Send initial message if provided
    if (widget.initialMessage.isNotEmpty) {
      Future.delayed(Duration(milliseconds: 500), () {
        sendMessage(context, widget.initialMessage, null);
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

      _updateSelectedModel();
      _loadConversation(widget.conversationId);

      setState(() {
        _shouldAutoScroll = true;
        _closeMessageWarning = false;
      });
    }
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

  _generateTitle(
    ConversationsNotifier conversationsNotifier,
    String conversationId,
  ) async {
    // call api.generateTitle and save it to the conversation
    var result = await _apiService.generateTitle(widget.conversationId);
    var title = result['title'] ?? null;
    var icon = result['icon'] ?? null;

    // save
    if (title != null) {
      conversationsNotifier.updateConversationMetaData(
        conversationId: widget.conversationId,
        title: title,
        icon: icon,
      );

      widget.updateMainTitle();
    }
  }

  @override
  void dispose() {
    _mainScrollController.dispose();
    _miniMapScrollController.dispose();
    _inputFocusNode.dispose();
    _messageController.dispose();
    super.dispose();
  }

  void _scrollListener() {
    final isNearBottom =
        _mainScrollController.position.pixels >=
        _mainScrollController.position.maxScrollExtent - 200;

    if (isNearBottom != _shouldAutoScroll) {
      setState(() {
        _shouldAutoScroll = isNearBottom;
      });
    }
  }

  void _scrollToBottom({bool force = false}) {
    Future.delayed(Duration(milliseconds: 160), () {
      if ((_shouldAutoScroll && !_isLoading) || force) {
        _mainScrollController.jumpTo(
          _mainScrollController.position.maxScrollExtent,
        );
      } else if (_shouldAutoScroll) {
        _mainScrollController.animateTo(
          _mainScrollController.position.maxScrollExtent,
          duration: Duration(milliseconds: 500),
          curve: Curves.easeOut,
        );
      }
    });
  }

  void _handleStop() {
    setState(() {
      _interrupted = true;
    });
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
          // TODO
          final bytes = await xFile.readAsBytes();
          final base64Data = base64Encode(bytes);

          setState(() {
            attachments.add(
              Attachment(
                type: 'file',
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
      await sendMessage(context, userMessage, null);
    }
  }

  Future<void> sendMessage(
    BuildContext context,
    String? userMessage,
    String? forceConvId,
  ) async {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    final balanceNotifier = ref.read(balanceProvider.notifier);

    print("Sending message: $userMessage");

    // Create new message object
    final newMessage =
        (userMessage != null && userMessage.isNotEmpty) ||
                attachments.isNotEmpty
            ? Message(
              role: "user",
              content: [
                MessageContent.text((userMessage ?? '')),
                ...attachments
                    .map(
                      (a) =>
                          a.type == "image_url"
                              ? MessageContent.image(a.content)
                              : MessageContent(type: a.type, text: a.content),
                    )
                    .toList(),
              ],
            )
            : null;

    List<Message> allToolResults = [];

    if (newMessage == null) {
      // check if last messages are tool_results
      final conversationsState = ref.read(conversationsProvider);
      final matches =
          conversationsState.conversations
              .where((conv) => conv.id == widget.conversationId)
              .toList();

      if (matches.isNotEmpty) {
        final currentConversation = matches.first;
        if (currentConversation.messages.isNotEmpty) {
          // Start from the last message and work backwards
          List<Message> toolResultChain = [];

          // Iterate backwards through messages
          for (int i = currentConversation.messages.length - 1; i >= 0; i--) {
            final message = currentConversation.messages[i];

            // Check if this message contains ONLY tool_results and nothing else
            bool containsOnlyToolResults =
                message.content.isNotEmpty &&
                message.content.every((c) => c.type == "tool_result");

            if (containsOnlyToolResults) {
              // Add this message to our chain
              toolResultChain.add(message);
            } else {
              // Chain is broken, stop iterating
              break;
            }
          }

          // Reverse the list to get chronological order if needed
          // toolResultChain = toolResultChain.reversed.toList();

          if (toolResultChain.isNotEmpty) {
            // Extract all tool calls from the chain
            for (var message in toolResultChain) {
              allToolResults.add(message);
            }
          }
        }
      }
    }

    setState(() {
      _isLoading = true;
      attachments.clear();
      _interrupted = false;
    });

    var currentConversationID = forceConvId ?? widget.conversationId;
    var isNewConversation = currentConversationID == "";
    var tokenPrice = 0;
    var modelReported = null;
    var toolHaveBeenUsed = false;
    List<ToolCall> toolUsedList = [];
    List<ToolCall> toolResultList = [];
    String cachedTitle = "";

    if (!isNewConversation && newMessage != null) {
      // Add message to Riverpod state
      conversationsNotifier.addMessage(
        conversationId: currentConversationID,
        message: newMessage,
      );
    }

    try {
      // Send message to API
      final stream = await _apiService.sendChatMessage(
        _miniAppSelected,
        currentConversationID,
        _modelSelected,
        newMessage != null ? [newMessage] : allToolResults,
        ({newConversationID, newConversationTitle}) {
          if (cachedTitle != newConversationTitle &&
              newConversationTitle != null) {
            conversationsNotifier.updateConversationMetaData(
              conversationId: newConversationID,
              title: newConversationTitle,
              modelSelected: _modelSelected,
            );

            cachedTitle = newConversationTitle;
          }

          if (newConversationID != currentConversationID) {
            currentConversationID = newConversationID;

            // create new conversation
            conversationsNotifier.createConversation(
              id: currentConversationID,
              modelSelected: _modelSelected,
              title: '',
              miniApp: _miniAppSelected,
            );

            if (isNewConversation && newMessage != null) {
              // Add message to Riverpod state
              conversationsNotifier.addMessage(
                conversationId: currentConversationID,
                message: newMessage,
              );
            }

            if (widget.setConversationID != null)
              widget.setConversationID!(currentConversationID, false);
          }
        },
        ({newTokenPrice, newModel}) {
          tokenPrice = newTokenPrice;
          modelReported = newModel;
        },
        ({toolUsed, toolResult}) {
          if (toolUsed != null) {
            toolHaveBeenUsed = true;
            toolUsedList.add(toolUsed);
          } else if (toolResult != null) {
            toolResultList.add(toolResult);
          }
        },
      );

      // Process streaming response
      _currentStreamedResponse = '';
      await for (final chunk in stream) {
        if (_interrupted) break;

        setState(() {
          _currentStreamedResponse += chunk;
          if (_currentStreamedResponse.length > 1500 &&
              _currentStreamedResponse.length < 2500) {
            _shouldAutoScroll = false;
          }
        });
      }

      // Process completed response
      var allContent = [
        MessageContent.text(_currentStreamedResponse),
        ...toolUsedList.map((tool) => MessageContent.tool(tool)),
        ...toolResultList.map((tool) => MessageContent.tool(tool)),
      ];

      final assistantMessage = Message(
        role: "assistant",
        content: allContent,
        totalTokens: tokenPrice,
        model: modelReported,
      );

      // Add assistant's response to Riverpod state
      conversationsNotifier.addMessage(
        conversationId: currentConversationID,
        message: assistantMessage,
      );

      // TTS for calls
      var stt = SpeechRecognitionService();
      var tts = TTSService();
      if (stt.isCall) {
        tts.speak(_currentStreamedResponse, () {
          if (stt.isCall) {
            if (isNewConversation) stt.dismissModal();
            stt.startRecording(context, autoStop: true, call: true);
          }
        }, queue: true);
      }

      if (newMessage != null) {
        genImage(
          _modelSelected.imageGen?.name ?? '',
          newMessage?.text,
          currentConversationID,
          conversationsNotifier,
          _apiService,
        );

        genImage(
          _modelSelected.imageGen?.name ?? '',
          _currentStreamedResponse,
          currentConversationID,
          conversationsNotifier,
          _apiService,
        );
      }

      setState(() {
        _currentStreamedResponse = '';
        _isLoading = false;
      });

      if (toolHaveBeenUsed && await processToolRequests()) {
        await sendMessage(context, "", currentConversationID);
      }

      if (isNewConversation) {
        _generateTitle(conversationsNotifier, currentConversationID);
      }

      balanceNotifier.refresh();

      if (isNewConversation) {
        if (widget.setConversationID != null) {
          widget.setConversationID!(currentConversationID, true);
        }
      }

      // Focus the input field for next message
      Future.delayed(Duration(milliseconds: 150), () {
        _inputFocusNode.requestFocus();
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
      setState(() => _isLoading = false);
    }
  }

  Future<bool> processToolRequests() async {
    // Get the conversation state
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    final conversationsState = ref.read(conversationsProvider);
    final matches =
        conversationsState.conversations
            .where((conv) => conv.id == widget.conversationId)
            .toList();

    if (matches.isEmpty) return false;
    final currentConversation = matches.first;

    if (currentConversation.messages.isEmpty) return false;

    // Get the last message (should be the assistant's response)
    final lastMessage = currentConversation.messages.last;

    // Check if there are any tool_use content items
    final toolUses =
        lastMessage.content.where((c) => c.type == "tool_use").toList();
    if (toolUses.isEmpty) return false;

    // Find tool uses that haven't been processed yet (no matching tool_result)
    final unprocessedTools =
        toolUses.where((toolUse) {
          return !currentConversation.messages.any(
            (message) => message.content.any(
              (content) =>
                  content.type == "tool_result" &&
                  content.toolCall?.id == toolUse.toolCall?.id,
            ),
          );
        }).toList();

    if (unprocessedTools.isEmpty) return false;

    // Process MCP client-side tools
    bool hasServerTools = false;

    for (final toolUse in unprocessedTools) {
      final toolCall = toolUse.toolCall;
      if (toolCall == null) continue;

      // Check if this is an MCP tool
      final mcpService = MCPService();
      final tools = mcpService.getToolList();

      final matchingTool = tools.firstWhere(
        (tool) => tool['name'] == toolCall.name,
        orElse: () => null,
      );

      if (matchingTool != null) {
        print("[MCP] Found MCP tool: ${toolCall.name}");
        // This is an MCP tool, process it
        try {
          // Parse arguments
          final args = jsonDecode(
            toolCall.arguments == "" ? "{}" : toolCall.arguments,
          );

          // Find the server name from the tool
          final serverName = MCPService().getToolServerName(toolCall.name);

          if (serverName != null) {
            // Execute the tool call
            await mcpService.serverManager
                .sendRequest(serverName, 'tools/call', {
                  'name': toolCall.name,
                  'arguments': args,
                })
                .then((response) {
                  // Create a tool result
                  final toolResult = ToolCall(
                    id: toolCall.id,
                    name: toolCall.name,
                    arguments: toolCall.arguments,
                    result: jsonEncode(response),
                  );

                  print("[MCP] Tool result: ${toolResult.result}");

                  // Update the last message to include the tool result
                  // final updatedContents = [...lastMessage.content, content];
                  final newMessage = Message(
                    role: "user",
                    content: MessageContent.complexTools(toolResult),
                    totalTokens: 0,
                  );

                  // Update the message in the conversation
                  conversationsNotifier.addMessage(
                    conversationId: widget.conversationId,
                    message: newMessage,
                  );

                  hasServerTools = true;
                })
                .catchError((error) {
                  // Handle error
                  final toolResult = ToolCall(
                    id: toolCall.id,
                    name: toolCall.name,
                    arguments: toolCall.arguments,
                    result: "Error: ${error.toString()}",
                  );

                  final content = MessageContent.tool(toolResult);

                  // Update the last message to include the tool result
                  // final updatedContents = [...lastMessage.content, content];
                  final newMessage = Message(
                    role: "user",
                    content: [content],
                    totalTokens: 0,
                  );

                  // Update the message in the conversation
                  conversationsNotifier.addMessage(
                    conversationId: widget.conversationId,
                    message: newMessage,
                  );

                  hasServerTools = true;
                });
          }
        } catch (e) {
          print('Error processing MCP tool: $e');
        }
      } else {
        print("[MCP] Server tool: ${toolCall.name}");
        // Not an MCP tool, must be a server-side tool
        hasServerTools = true;
      }
    }

    print(
      "[MCP] Processed ${unprocessedTools.length} tool requests. " +
          (hasServerTools
              ? "Server tools detected."
              : "No server tools detected."),
    );

    // Return true if there are any server-side tools that need processing
    return hasServerTools;
  }

  // Function to build a message list
  Widget buildMessageList({
    required List<Message> messages,
    required ScrollController controller,
    bool mini = false,
    EdgeInsetsGeometry? padding,
  }) {
    var l = SuperListView.builder(
      controller: controller,
      cacheExtent: 100,
      padding: padding ?? const EdgeInsets.all(16.0),
      itemCount:
          messages.length + (_currentStreamedResponse.isNotEmpty ? 1 : 0),
      itemBuilder: (context, index) {
        Message message;
        if (index < messages.length) {
          message = messages[index];
        } else {
          message = Message(
            role: "assistant",
            content: [MessageContent.text(_currentStreamedResponse)],
          );
        }

        var alignMess =
            sanitizeMessages(message.text) != ""
                ? AnimatedMessageBox(
                  iconURL: _miniAppSelected?.iconURL,
                  mini: mini, // Use mini parameter
                  message: message,
                  text: sanitizeMessages(message.text),
                  isBot: message.isBot,
                  isLoading:
                      _isLoading &&
                      index ==
                          (messages.length +
                                  (_currentStreamedResponse.isNotEmpty
                                      ? 1
                                      : 0)) -
                              1,
                )
                : null;

        return Align(
          alignment:
              message.isBot ? Alignment.centerLeft : Alignment.centerRight,
          child: Column(
            crossAxisAlignment:
                message.isBot
                    ? CrossAxisAlignment.start
                    : CrossAxisAlignment.end,
            children: [
              ...message.content
                  .where((c) => c.type != "text")
                  .map(
                    (attach) => AttachmentViewer(
                      mini: mini,
                      toolCall: attach.toolCall,
                      loading: _isLoading && index == messages.length - 1,
                      attachment: Attachment(
                        type: attach.type,
                        content:
                            (attach.type == "image_url"
                                ? attach.imageUrl?.url
                                : attach.text) ??
                            '',
                      ),
                      removeAttachment: _removeAttachment,
                      editMode: false,
                    ),
                  )
                  .toList(),
              if (alignMess != null) alignMess,
              if (!mini)
                ...message.content
                    .where((c) => c.type == "tool_use")
                    .map(
                      (attach) => ToolCallBadge(
                        toolCall: attach.toolCall!,
                        isLoading: _isLoading && index == messages.length - 1,
                        // if next message contains a tool_result
                        result:
                            (index + 1 < messages.length &&
                                    messages
                                        .elementAt(index + 1)
                                        .content
                                        .any(
                                          (c) =>
                                              c.type == "tool_result" &&
                                              c.toolCall!.id ==
                                                  attach.toolCall!.id,
                                        ))
                                ? messages
                                    .elementAt(index + 1)
                                    .content
                                    .firstWhereOrNull(
                                      (c) =>
                                          c.type == "tool_result" &&
                                          c.toolCall!.id == attach.toolCall!.id,
                                    )
                                : null,
                      ),
                    )
                    .toList(),
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

    if (messages.isNotEmpty) _scrollToBottom();

    // Get bottom padding to ensure minimap doesn't go under input box
    final bottomPadding = 0.0;

    // Build main content and minimap content using the shared function
    final mainContent = buildMessageList(
      messages: messages,
      controller: _mainScrollController,
    );

    final miniMapContent = buildMessageList(
      messages: messages,
      controller: _miniMapScrollController,
      mini: true,
      padding: EdgeInsets.only(
        left: 4.0,
        right: 4.0,
        top: 4.0,
        bottom:
            bottomPadding, // Add bottom padding to prevent going under input
      ),
    );

    final isDarkMode = Theme.of(context).brightness == Brightness.dark;

    // Combine both scrolling methods
    Widget chatContent = MiddleClickScroller(
      scrollController: _mainScrollController,
      iconColor: Theme.of(context).primaryColor,
      child: MiniMap(
        enabled: !widget.isMobile && !kIsWeb,
        mainScrollController: _mainScrollController,
        miniMapScrollController: _miniMapScrollController,
        miniMapContent: miniMapContent,
        overlayColor:
            Theme.of(context).brightness == Brightness.dark
                ? Theme.of(context).textTheme.bodySmall?.color ?? Colors.red
                : Theme.of(context).primaryColor,
        miniMapWidth: 140,
        overlayHeight: 80,
        child: mainContent,
      ),
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
                              _miniAppSelected!.InitialMessage?['en'] ??
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
              isLoading: _isLoading,
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

    return Stack(
      children: [
        widget.conversationId.isEmpty && _miniAppSelected != null
            ? Positioned.fill(
              child: Center(
                child: SingleChildScrollView(child: newMessageScreen),
              ),
            )
            : newMessageScreen,

        // if last message of messages has message.TokenPrice > 1000
        if (!_closeMessageWarning &&
            messages.isNotEmpty &&
            messages.last.totalTokens != null &&
            messages.last.totalTokens! > 40000 &&
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
                        " credits",
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
        if (!_shouldAutoScroll)
          Positioned(
            right: 16,
            bottom: 80,
            child: FloatingActionButton(
              mini: true,
              onPressed: () => _scrollToBottom(force: true),
              child: const Icon(Icons.arrow_downward),
            ),
          ),
      ],
    );
  }
}
