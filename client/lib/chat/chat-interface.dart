import 'package:flutter/foundation.dart';
import 'package:plurality/chat/attachments.dart';
import './snackbar.dart';
import 'package:flutter/material.dart';
import 'dart:convert';
import '../utils/types.dart';
import '../api/api.dart';
import '../api/service.dart';
import 'package:flutter/services.dart';
import './AnimatedMessageBox.dart';
import 'package:image_picker/image_picker.dart';
import 'package:file_picker/file_picker.dart';
import 'package:top_snackbar_flutter/top_snack_bar.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import './image.dart';
import './image-gen.dart';
import './input.dart';
import '../utils/file-types.dart';
import './minimap.dart'; // MiniMap component
import './middle-click.dart'; // MiddleClickScroller
import 'package:super_sliver_list/super_sliver_list.dart';

class ChatInterface extends ConsumerStatefulWidget {
  final String conversationId;
  final VoidCallback? onConversationUpdated;
  final String initialMessage;
  final bool isMobile;
  final Function(String, bool)? setConversationID;

  const ChatInterface({
    super.key,
    required this.conversationId,
    this.onConversationUpdated,
    this.initialMessage = '',
    this.setConversationID,
    required this.isMobile,
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

  ModelSelected _modelSelected = ModelSelected();

  @override
  void initState() {
    super.initState();
    _mainScrollController.addListener(_scrollListener);

    _updateSelectedModel();
    _loadConversation(widget.conversationId);

    // Send initial message if provided
    if (widget.initialMessage.isNotEmpty) {
      Future.delayed(Duration(milliseconds: 500), () {
        sendMessage(context, widget.initialMessage);
      });
    }
  }

  @override
  void didUpdateWidget(ChatInterface oldWidget) {
    super.didUpdateWidget(oldWidget);

    // Check if the conversationId has changed
    if (oldWidget.conversationId != widget.conversationId) {
      _updateSelectedModel();
      _loadConversation(widget.conversationId);

      setState(() {
        _shouldAutoScroll = true;
      });
    }
  }

  // Extract the model selection logic to a separate method
  void _updateSelectedModel() {
    final conversationsState = ref.read(conversationsProvider);
    final conversationsNotifier = ref.read(conversationsProvider.notifier);

    final matches =
        conversationsState
            .where((conv) => conv.id == widget.conversationId)
            .toList();

    if (matches.isNotEmpty) {
      _modelSelected = matches.first.modelSelected;
    } else {
      _modelSelected = conversationsNotifier.getSelectedModel();
    }
  }

  // Extract the model selection logic to a separate method
  void _loadConversation(String id) {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    conversationsNotifier.loadConversation(id);
  }

  void _setSelectedModel(ModelSelected modelSelected) {
    setState(() {
      _modelSelected = modelSelected;
    });

    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    conversationsNotifier.saveSelectedModel(modelSelected);
  }

  void _generateTitle(
    ConversationsNotifier conversationsNotifier,
    String conversationId,
  ) async {
    // call api.generateTitle and save it to the conversation
    var title = await _apiService.generateTitle(widget.conversationId);

    // save
    if (title != null) {
      conversationsNotifier.updateConversationMetaData(
        conversationId: widget.conversationId,
        title: title,
      );
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
      if (_shouldAutoScroll || force) {
        _mainScrollController.animateTo(
          _mainScrollController.position.maxScrollExtent,
          duration: Duration(milliseconds: (force ? 500 : 90)),
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
        maxWidth: 2048,
        maxHeight: 2048,
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
      final userMessage = _messageController.text;
      _messageController.clear();
      await sendMessage(context, userMessage);
    }
  }

  Future<void> sendMessage(BuildContext context, String userMessage) async {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);

    // Create new message object
    final newMessage = Message(
      role: "user",
      content: [
        MessageContent.text(userMessage),
        // if (_selectedImage != null) MessageContent.image(_selectedImage!),
        ...attachments
            .map(
              (a) =>
                  a.type == "image_url"
                      ? MessageContent.image(a.content)
                      : MessageContent(type: a.type, text: a.content),
            )
            .toList(),
      ],
    );

    setState(() {
      _isLoading = true;
      attachments.clear();
      _interrupted = false;
    });

    var isNewConversation = widget.conversationId == "";
    var currentConversationID = widget.conversationId;

    try {
      // Send message to API
      final stream = await _apiService.sendChatMessage(
        widget.conversationId,
        _modelSelected,
        newMessage,
        ({newConversationID, newConversationTitle}) {
          conversationsNotifier.updateConversationMetaData(
            conversationId: newConversationID,
            title: newConversationTitle,
            modelSelected: _modelSelected,
          );

          if (newConversationID != currentConversationID) {
            currentConversationID = newConversationID;

            // create new conversation
            conversationsNotifier.createConversation(
              id: currentConversationID,
              modelSelected: _modelSelected,
              title: '',
            );

            if (isNewConversation) {
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
      );

      if (!isNewConversation) {
        // Add message to Riverpod state
        conversationsNotifier.addMessage(
          conversationId: currentConversationID,
          message: newMessage,
        );
      }

      // Process streaming response
      _currentStreamedResponse = '';
      await for (final chunk in stream) {
        if (_interrupted) break;

        setState(() {
          _currentStreamedResponse += chunk;
          if (_currentStreamedResponse.length > 1800) {
            _shouldAutoScroll = false;
          }
        });
      }

      // Process completed response
      final assistantMessage = Message(
        role: "assistant",
        content: [MessageContent.text(_currentStreamedResponse)],
      );

      // Add assistant's response to Riverpod state
      conversationsNotifier.addMessage(
        conversationId: currentConversationID,
        message: assistantMessage,
      );

      if (isNewConversation) {
        _generateTitle(conversationsNotifier, currentConversationID);
      }

      genImage(
        _modelSelected.imageGen?.name ?? '',
        newMessage.text,
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

      setState(() {
        _currentStreamedResponse = '';
        _isLoading = false;
      });

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

  // Function to build a message list
  Widget buildMessageList({
    required List<Message> messages,
    required ScrollController controller,
    bool mini = false,
    EdgeInsetsGeometry? padding,
  }) {
    return SuperListView.builder(
      controller: controller,
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
              if (message.text != "")
                AnimatedMessageBox(
                  mini: mini, // Use mini parameter
                  text: message.text,
                  isBot: message.isBot,
                  isLoading:
                      _isLoading &&
                      index ==
                          (messages.length +
                                  (_currentStreamedResponse.isNotEmpty
                                      ? 1
                                      : 0)) -
                              1,
                ),
            ],
          ),
        );
      },
    );
  }

  @override
  Widget build(BuildContext context) {
    final conversationsState = ref.watch(conversationsProvider);
    final currentConversation = conversationsState.firstWhere(
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
    final bottomPadding = 0.0; /*
        MediaQuery.of(context).padding.bottom +
        80.0; // Input box height estimation
*/
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

    // Combine both scrolling methods
    Widget chatContent = MiddleClickScroller(
      scrollController: _mainScrollController,
      iconColor: Theme.of(context).primaryColor,
      child: MiniMap(
        enabled: !widget.isMobile && !kIsWeb,
        mainScrollController: _mainScrollController,
        miniMapScrollController: _miniMapScrollController,
        miniMapContent: miniMapContent,
        overlayColor: Theme.of(context).primaryColor,
        miniMapWidth: 140,
        overlayHeight: 80,
        child: mainContent,
      ),
    );

    return Stack(
      children: [
        Column(
          mainAxisAlignment: MainAxisAlignment.center,
          crossAxisAlignment: CrossAxisAlignment.center,
          children: [
            if (widget.conversationId.isNotEmpty) Expanded(child: chatContent),

            if (widget.conversationId.isEmpty)
              Center(
                child: Text(
                  'Start a new conversation',
                  style: Theme.of(context).textTheme.headlineSmall,
                ),
              ),

            // Input Container
            Container(
              margin: const EdgeInsets.symmetric(horizontal: 16.0),
              padding:
                  widget.conversationId.isNotEmpty
                      ? const EdgeInsets.symmetric(
                        horizontal: 12.0,
                        vertical: 8.0,
                      )
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
                        color: Colors.white,
                        borderRadius: BorderRadius.only(
                          topLeft: Radius.circular(8.0),
                          topRight: Radius.circular(8.0),
                        ),
                        border: Border(
                          top: BorderSide(color: Color(0xFFEEEEEE)),
                          left: BorderSide(color: Color(0xFFEEEEEE)),
                          right: BorderSide(color: Color(0xFFEEEEEE)),
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
                  attachments: attachments,
                  conversationId: widget.conversationId,
                ),
              ),
            ),
          ],
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
