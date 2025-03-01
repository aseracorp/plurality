import './snackbar.dart';
import 'package:flutter/material.dart';
import 'dart:convert';
import 'dart:io';
import '../utils/types.dart';
import '../auth/auth-service.dart';
import '../api/index.dart';
import './AnimatedMessageBox.dart';
import 'package:image_picker/image_picker.dart';
import 'package:top_snackbar_flutter/top_snack_bar.dart';

class ChatInterface extends StatefulWidget {
  final String conversationId;
  final VoidCallback? onConversationUpdated;
  final String initialMessage;

  const ChatInterface({
    super.key,
    required this.conversationId,
    this.onConversationUpdated,
    this.initialMessage = '',
  });

  @override
  State<ChatInterface> createState() => _ChatInterfaceState();
}

class _ChatInterfaceState extends State<ChatInterface> {
  final apiService = ApiService();
  final FocusNode _inputFocusNode = FocusNode();
  final _formKey = GlobalKey<FormState>();
  final TextEditingController _messageController = TextEditingController();
  final List<Message> _messages = [];
  String _currentStreamedResponse = '';
  String _currentTitle = '';
  bool _isLoading = false;
  String conversationId = '';

  final ScrollController _scrollController = ScrollController();
  bool _shouldAutoScroll = true;
  bool _interupted = false;

  final ImagePicker _imagePicker = ImagePicker();
  String? _selectedImage;

  Future<void> loadConversation() async {
    // final conv = await ConversationStorage.getConversation(
    //   conversationId,
    // );
    // setState(() {
    //   _currentTitle = conv?.title ?? '';
    //   _messages.clear();
    //   _messages.addAll(conv?.messages ?? []);
    // });
  }

  Future<void> _pickImage2() async {
    return _pickImage(source: ImageSource.camera);
  }

  Future<void> _pickImage({source = ImageSource.gallery}) async {
    try {
      final XFile? image = await _imagePicker.pickImage(
        source: source,
        maxWidth: 2048,
        maxHeight: 2048,
      );

      if (image != null) {
        final bytes = await image.readAsBytes();
        final mimeType =
            image.mimeType ?? 'image/jpeg'; // Default to jpeg if type unknown
        final base64Data = base64Encode(bytes);
        setState(() {
          _selectedImage = 'data:$mimeType;base64,$base64Data';
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

  @override
  void initState() {
    super.initState();
    conversationId = widget.conversationId;
    _scrollController.addListener(_scrollListener);
    loadConversation();

    if (widget.initialMessage.isNotEmpty) {
      Future.delayed(Duration(milliseconds: 500), () {
        sendMessage(context, widget.initialMessage);
      });
    }
  }

  @override
  void dispose() {
    _scrollController.dispose();
    _inputFocusNode.dispose();

    super.dispose();
  }

  void _scrollListener() {
    if (_scrollController.position.pixels >=
        _scrollController.position.maxScrollExtent - 200) {
      if (!_shouldAutoScroll)
        setState(() {
          _shouldAutoScroll = true;
        });
    } else if (!_scrollController.position.outOfRange) {
      if (_shouldAutoScroll)
        setState(() {
          _shouldAutoScroll = false;
        });
    }
  }

  void _scrollToBottom({bool force = false}) {
    // delay the scroll to bottom to allow the list to update
    Future.delayed(Duration(milliseconds: 150), () {
      if (_shouldAutoScroll || force) {
        _scrollController.animateTo(
          _scrollController.position.maxScrollExtent,
          duration: Duration(milliseconds: (force ? 500 : 100)),
          curve: Curves.easeOut,
        );
      }
    });
  }

  void _handleStop() {
    _interupted = true;
  }

  Future<void> _setTitle(String userMessage) async {
    // // if first message, send a title request
    // if (_messages.length == 1) {
    //   final stream = await getChatTitle(userMessage);

    //   await for (final chunk in stream) {
    //     setState(() {
    //       _currentTitle += chunk;
    //       if (_currentTitle.startsWith('#')) {
    //         _currentTitle = _currentTitle.substring(1);
    //       }
    //     });
    //   }

    //   await ConversationStorage.saveTitle(conversationId, _currentTitle);

    //   widget.onConversationUpdated?.call();
    // }
  }

  Future<void> _handleSubmit(BuildContext context) async {
    if (_formKey.currentState!.validate()) {
      final userMessage = _messageController.text;
      _messageController.clear();

      await sendMessage(context, userMessage);
    }
  }

  Future<void> sendMessage(BuildContext context, String userMessage) async {
    _setTitle(userMessage);

    // await ConversationStorage.saveMessage(
    //   conversationId,
    //   Message(
    //     text: userMessage,
    //     role: "user",
    //     imageBase64: _selectedImage ?? '',
    //   ),
    // );

    var newmessage = Message(
      role: "user",
      content: [
        MessageContent.text(userMessage),
        if (_selectedImage != null) MessageContent.image(_selectedImage!),
      ],
    );

    setState(() {
      _messages.add(newmessage);
      _isLoading = true;
      _selectedImage = null; // Clear the selected image after sending
    });

    try {
      // if start with /image, generate an image instead
      // if (userMessage.startsWith('/image')) {
      //   final imageResult = await generateImage(userMessage.substring(7));

      //   _currentStreamedResponse = '';

      //   setState(() {
      //     _messages.add(
      //       Message(
      //         text: 'Generated image in ${imageResult.time}s',
      //         role: "assistant",
      //         imageBase64: imageResult.base64,
      //       ),
      //     );
      //     _isLoading = false;
      //     _selectedImage = null;
      //   });

      //   await ConversationStorage.saveMessage(
      //     conversationId,
      //     Message(
      //       text: 'Generated image',
      //       role: "assistant",
      //       imageBase64: imageResult.base64,
      //     ),
      //   );

      //   Future.delayed(Duration(milliseconds: 150), () {
      //     _inputFocusNode.requestFocus();
      //   });

      //   return;
      // }

      final stream = await apiService.sendChatMessage(
        conversationId,
        newmessage,
        ({newConversationID}) {
          setState(() {
            conversationId = newConversationID;
          });

          // TODO: Save conversation ID
          // await ConversationStorage.saveConversationID(
          //   conversationId,
          // );
          // OR SOMETHING LIKE THAT
        },
      );

      _currentStreamedResponse = '';
      await for (final chunk in stream) {
        if (_interupted) {
          _interupted = false;
          break;
        }

        // print('Chunk: $chunk');

        setState(() {
          _currentStreamedResponse += chunk;

          if (_currentStreamedResponse.length > 1800) {
            _shouldAutoScroll = false;
          }
        });
      }

      // check if the message contains /image {prompt} \n it can be within a paragraph
      final imageMatch = RegExp(
        r'/image ([^\n]+)',
      ).firstMatch(_currentStreamedResponse);

      setState(() {
        _messages.add(
          Message(
            role: "assistant",
            content: [
              MessageContent.text(_currentStreamedResponse),
              if (imageMatch != null)
                MessageContent.image(
                  'data:image/png;base64,${base64Encode(utf8.encode(''))}',
                ),
            ],
          ),
        );

        _currentStreamedResponse = '';
        _isLoading = false;
      });

      // await ConversationStorage.saveMessage(
      //   conversationId,
      //   Message(text: _messages.last.text, role: "assistant"),
      // );

      // if (imageMatch != null) {
      //   final imageResult = await generateImage(imageMatch.group(1)!);

      //   setState(() {
      //     _messages.add(
      //       Message(
      //         text: 'Generated image in ${imageResult.time}s',
      //         role: "assistant",
      //         imageBase64: imageResult.base64,
      //       ),
      //     );
      //   });

      //   await ConversationStorage.saveMessage(
      //     conversationId,
      //     Message(
      //       text: 'Generated image',
      //       role: "assistant",
      //       imageBase64: imageResult.base64,
      //     ),
      //   );
      // }

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

  @override
  Widget build(BuildContext context) {
    _scrollToBottom();

    return Stack(
      children: [
        Column(
          children: [
            Expanded(
              child: ListView.builder(
                controller: _scrollController,
                padding: const EdgeInsets.all(16.0),
                itemCount:
                    _messages.length +
                    (_currentStreamedResponse.isNotEmpty ? 1 : 0),
                itemBuilder: (context, index) {
                  Message message;
                  if (index < _messages.length) {
                    message = _messages[index];
                  } else {
                    message = Message(
                      role: "assistant",
                      content: [MessageContent.text(_currentStreamedResponse)],
                    );
                  }

                  return Align(
                    alignment:
                        message.isBot
                            ? Alignment.centerLeft
                            : Alignment.centerRight,
                    child: Column(
                      crossAxisAlignment:
                          message.isBot
                              ? CrossAxisAlignment.start
                              : CrossAxisAlignment.end,
                      children: [
                        if (message.content.any((c) => c.type == "image"))
                          ...message.content
                              .where((c) => c.type == "image")
                              .map(
                                (imageContent) => Padding(
                                  key: ValueKey(
                                    'image_${index}_${imageContent.imageUrl.hashCode}',
                                  ),
                                  padding: const EdgeInsets.only(bottom: 8.0),
                                  child: ClipRRect(
                                    borderRadius: BorderRadius.circular(8.0),
                                    child: Image.memory(
                                      base64Decode(
                                        (imageContent.imageUrl?.url
                                                .split(",")
                                                .last) ??
                                            '',
                                        // TODO: Empty image
                                      ),
                                      width: 200,
                                      fit: BoxFit.cover,
                                      cacheWidth: 200,
                                      gaplessPlayback: true,
                                    ),
                                  ),
                                ),
                              )
                              .toList(),
                        AnimatedMessageBox(
                          text: message.text,
                          isBot: message.isBot,
                          isLoading:
                              _isLoading &&
                              index ==
                                  (_messages.length +
                                          (_currentStreamedResponse.isNotEmpty
                                              ? 1
                                              : 0)) -
                                      1,
                        ),
                      ],
                    ),
                  );
                },
              ),
            ),
            Container(
              padding: const EdgeInsets.all(8.0),
              decoration: BoxDecoration(
                color: Colors.white,
                border: Border(top: BorderSide(color: Colors.grey.shade300)),
              ),
              child: Form(
                key: _formKey,
                child: Row(
                  children: [
                    Expanded(
                      child: ConstrainedBox(
                        constraints: BoxConstraints(maxHeight: 300),
                        child: TextFormField(
                          controller: _messageController,
                          focusNode: _inputFocusNode,
                          decoration: InputDecoration(
                            hintText:
                                _selectedImage != null
                                    ? 'Image selected. Add a message...'
                                    : 'Type a message...',
                            border: OutlineInputBorder(),
                          ),
                          maxLines: null,
                          minLines: 1,
                          enabled: !_isLoading,
                          textInputAction: TextInputAction.send,
                          validator: (value) {
                            if ((value == null || value.trim().isEmpty) &&
                                _selectedImage == null) {
                              return 'Please enter a message or select an image';
                            }
                            return null;
                          },
                          onFieldSubmitted: (_) => _handleSubmit(context),
                        ),
                      ),
                    ),
                    const SizedBox(width: 8.0),
                    IconButton(
                      onPressed: _isLoading ? null : _pickImage2,
                      icon: Icon(
                        Icons.camera_alt,
                        color: _isLoading ? Colors.grey : Color(0xffee4654),
                      ),
                    ),
                    IconButton(
                      onPressed: _isLoading ? null : _pickImage,
                      icon: Icon(
                        Icons.image,
                        color: _isLoading ? Colors.grey : Color(0xffee4654),
                      ),
                    ),
                    IconButton(
                      onPressed:
                          () =>
                              _isLoading
                                  ? _handleStop()
                                  : _handleSubmit(context),
                      icon:
                          _isLoading
                              ? const Icon(Icons.stop, color: Color(0xffee4654))
                              : const Icon(
                                Icons.send,
                                color: Color(0xffee4654),
                              ),
                    ),
                  ],
                ),
              ),
            ),
          ],
        ),
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
