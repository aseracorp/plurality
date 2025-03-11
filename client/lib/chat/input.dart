import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:image_picker/image_picker.dart';
import 'dart:convert';
import 'dart:io';
import './model-picker.dart';
import 'package:speech_to_text/speech_to_text.dart' as stt;
import 'package:flutter/foundation.dart';
import '../utils/types.dart';
import './attachments.dart';

class InputBox extends StatefulWidget {
  // Required parameters
  final TextEditingController messageController;
  final Future<void> Function(BuildContext) onSend;
  final void Function(ModelSelected) setSelectedModel;
  final FocusNode? focusNode;
  final bool isLoading;
  final Future<void> Function({ImageSource source}) pickImage;
  final Future<void> Function() pickFile;
  final List<Attachment> attachments;
  final String conversationId;
  final String hintText;
  final Color accentColor;
  final FormFieldValidator<String>? validator;
  final FocusNode inputFocusNode;
  final void Function(Attachment? attachment) removeAttachment;
  final void Function(Attachment attachment) addAttachment;
  final void Function() handleStop;
  final bool isMobile;
  final ModelSelected selectedModel;

  const InputBox({
    Key? key,
    required this.messageController,
    required this.onSend,
    this.focusNode,
    required this.setSelectedModel,
    this.isLoading = false,
    required this.selectedModel,
    required this.handleStop,
    required this.pickImage,
    required this.pickFile,
    required this.isMobile,
    required this.attachments,
    required this.addAttachment,
    this.conversationId = '',
    this.hintText = 'Message...',
    required this.removeAttachment,
    required this.inputFocusNode,
    this.accentColor = const Color(0xffee4654),
    this.validator,
  }) : super(key: key);

  @override
  State<InputBox> createState() => _InputBoxState();
}

class _InputBoxState extends State<InputBox> {
  // Internal state
  static String _previousText = '';
  late stt.SpeechToText _speech;
  bool _isListening = false;
  String _currentText = '';

  @override
  void initState() {
    super.initState();
    _speech = stt.SpeechToText();
    widget.messageController.addListener(_onTextChanged);
  }

  void _onTextChanged() {
    String currentText = widget.messageController.text;

    // Check if text was pasted (significant increase in length)
    if (currentText.length > _previousText.length + 250) {
      // Get the pasted content
      String pastedContent = currentText.substring(_previousText.length);

      // Process the pasted content
      String processedContent = _processText(pastedContent);

      // Replace the pasted content with the processed version
      String newText = _previousText + processedContent;

      // Update the controller with the processed text
      widget.messageController.value = TextEditingValue(
        text: newText,
        selection: TextSelection.collapsed(offset: newText.length),
      );
    }

    _previousText = widget.messageController.text;
  }

  Future<void> _listen() async {
    if (!_isListening) {
      bool available = await _speech.initialize(
        onStatus: (status) {
          if (status == 'done' || status == 'notListening') {
            setState(() {
              _isListening = false;
            });
          }
        },
        onError: (error) {
          setState(() {
            _isListening = false;
          });
        },
      );

      if (available) {
        setState(() {
          _isListening = true;
        });

        _speech.listen(
          onResult: (result) {
            setState(() {
              _currentText = result.recognizedWords;
              // Update the text field with the recognized speech
              widget.messageController.text = _currentText;
            });
          },
        );
      }
    } else {
      setState(() {
        _isListening = false;
      });
      _speech.stop();
    }
  }

  String SummarizeSelectedModel(ModelSelected selectedModel) {
    String res = "";
    if (selectedModel.text != null) {
      res += selectedModel.text!.name;
    }

    // if one of the attachments is an image use vision model
    for (var attachment in widget.attachments) {
      if (attachment.type == "image_url") {
        if (selectedModel.vision != null) {
          res += selectedModel.vision!.name;
        }
        break;
      }
    }

    // split by / and return last
    List<String> parts = res.split("/");
    if (parts.length > 1) {
      res = parts[parts.length - 1];
    }

    // split by - and return first 3
    parts = res.split("-");
    if (parts.length > 2) {
      res = parts[0] + "-" + parts[1];
    }

    // seek XB (for size) and version (x.X) to add
    for (int i = 2; i < parts.length; i++) {
      if (RegExp(r'\d+B').hasMatch(parts[i])) {
        res += "-" + parts[i];
      } else if (RegExp(r'\d\.\d').hasMatch(parts[i])) {
        res += "-" + parts[i];
      } else if (RegExp(r'\d').hasMatch(parts[i])) {
        res += "-" + parts[i];
      }
    }

    return res;
  }

  String _processText(String text) {
    widget.addAttachment(Attachment(type: "snippet", content: text));
    return "";
  }

  void _handleSubmit(BuildContext context) {
    widget.onSend(context);
  }

  Widget getPlus() {
    return Row(
      children: [
        IconButton(
          onPressed: () => widget.pickImage(source: ImageSource.camera),
          icon: const Icon(Icons.camera_alt, color: Color(0xffee4654)),
        ),
        IconButton(
          onPressed: () => widget.pickImage(source: ImageSource.gallery),
          icon: const Icon(Icons.photo, color: Color(0xffee4654)),
        ),
        IconButton(
          onPressed: widget.pickFile,
          icon: const Icon(Icons.attach_file, color: Color(0xffee4654)),
        ),
        // pick AI model
        ElevatedButton(
          style: ElevatedButton.styleFrom(),
          onPressed: () {
            showDialog(
              context: context,
              builder: (BuildContext context) {
                return ModelSelectionModal(
                  selectedModel: widget.selectedModel,
                  onModelSelected: (ModelSelected models) {
                    widget.setSelectedModel(models);
                  },
                );
              },
            );
          },
          child: Row(
            children: [
              Icon(Icons.smart_toy),
              VerticalDivider(),
              Text(
                SummarizeSelectedModel(widget.selectedModel),
                style: TextStyle(fontSize: 10),
              ),
            ],
          ),
        ),
      ],
    );
  }

  KeyEventResult _handleKeyEvent(FocusNode node, KeyEvent event) {
    if (event is KeyDownEvent && event.logicalKey == LogicalKeyboardKey.enter) {
      if (HardwareKeyboard.instance.isShiftPressed) {
        return KeyEventResult.ignored;
      } else {
        _handleSubmit(context);
        return KeyEventResult.handled;
      }
    }
    return KeyEventResult.ignored;
  }

  @override
  Widget build(BuildContext context) {
    bool supportSST =
        kIsWeb || Platform.isAndroid || Platform.isIOS || Platform.isMacOS;

    return Container(
      width: double.infinity,
      constraints: BoxConstraints(
        maxWidth: widget.conversationId == "" ? 600 : double.infinity,
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Container(
            width: double.infinity,
            child: Row(
              children: [
                for (var attachment in widget.attachments ?? [])
                  AttachmentViewer(
                    attachment: attachment,
                    removeAttachment: widget.removeAttachment,
                    editMode: true,
                  ),
              ],
            ),
          ),
          // if (widget.isMobile)
          getPlus(),
          Container(
            width: double.infinity,
            child: Row(
              children: [
                Expanded(
                  child: ConstrainedBox(
                    constraints: BoxConstraints(maxHeight: 300),
                    child: Focus(
                      onKeyEvent: _handleKeyEvent,
                      child: TextFormField(
                        controller: widget.messageController,
                        focusNode: widget.inputFocusNode,
                        decoration: InputDecoration(
                          hintText: 'Message...',
                          border: null, // OutlineInputBorder(),
                        ),
                        maxLines: null,
                        minLines: 1,
                        autofocus: widget.conversationId.isNotEmpty,
                        enabled: !widget.isLoading,
                        keyboardType: TextInputType.multiline,
                        textInputAction: TextInputAction.newline,
                        validator: (value) {
                          if ((value == null || value.trim().isEmpty)) {
                            return 'Please enter a message';
                          }
                          return null;
                        },
                      ),
                    ),
                  ),
                ),
                const SizedBox(width: 8.0),
                // Speech-to-text microphone button
                if (supportSST)
                  IconButton(
                    onPressed: _listen,
                    icon: Icon(
                      _isListening ? Icons.mic : Icons.mic_none,
                      color: Color(0xffee4654),
                    ),
                  ),
                // Send or Stop button
                IconButton(
                  onPressed:
                      widget.isLoading
                          ? widget.handleStop
                          : () => _handleSubmit(context),
                  icon:
                      widget.isLoading
                          ? Stack(
                            alignment: Alignment.center,
                            children: [
                              SizedBox(
                                width: 24,
                                height: 24,
                                child: CircularProgressIndicator(
                                  strokeWidth: 2,
                                  valueColor: AlwaysStoppedAnimation<Color>(
                                    Color(0xffee4654),
                                  ),
                                ),
                              ),
                              const Icon(Icons.stop, color: Color(0xffee4654)),
                            ],
                          )
                          : const Icon(Icons.send, color: Color(0xffee4654)),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }
}
