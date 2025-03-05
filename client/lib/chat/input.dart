import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:image_picker/image_picker.dart';
import 'dart:convert';
import 'dart:io';
import './model-picker.dart';

import '../utils/types.dart';
import './attachments.dart';

class InputBox extends StatelessWidget {
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

  // Internal state
  static String _previousText = '';

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

  void _handleSubmit(BuildContext context) {
    onSend(context);
  }

  void _onTextChanged() {
    String currentText = messageController.text;

    // Check if text was pasted (significant increase in length)
    if (currentText.length > _previousText.length + 250) {
      // Get the pasted content
      String pastedContent = currentText.substring(_previousText.length);

      // Process the pasted content
      String processedContent = _processText(pastedContent);

      // Replace the pasted content with the processed version
      String newText = _previousText + processedContent;

      // Update the controller with the processed text
      messageController.value = TextEditingValue(
        text: newText,
        selection: TextSelection.collapsed(offset: newText.length),
      );
    }

    _previousText = messageController.text;
  }

  String SummarizeSelectedModel(ModelSelected selectedModel) {
    String res = "";
    if (selectedModel.text != null) {
      res += selectedModel.text!.name;
    }

    // if one of the attachments is an image use vision model
    for (var attachment in attachments) {
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
    addAttachment(Attachment(type: "snippet", content: text));
    return "";
  }

  @override
  Widget build(BuildContext context) {
    messageController.addListener(_onTextChanged);

    Widget getPlus() {
      return Row(
        children: [
          IconButton(
            onPressed: () => pickImage(source: ImageSource.camera),
            icon: const Icon(Icons.camera_alt, color: Color(0xffee4654)),
          ),
          IconButton(
            onPressed: () => pickImage(source: ImageSource.gallery),
            icon: const Icon(Icons.photo, color: Color(0xffee4654)),
          ),
          IconButton(
            onPressed: pickFile,
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
                    selectedModel: selectedModel,
                    onModelSelected: (ModelSelected models) {
                      setSelectedModel(models);
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
                  SummarizeSelectedModel(selectedModel),
                  style: TextStyle(fontSize: 10),
                ),
              ],
            ),
          ),
        ],
      );
    }

    KeyEventResult _handleKeyEvent(FocusNode node, KeyEvent event) {
      if (event is KeyDownEvent &&
          event.logicalKey == LogicalKeyboardKey.enter) {
        if (HardwareKeyboard.instance.isShiftPressed) {
          return KeyEventResult.ignored;
        } else {
          _handleSubmit(context);
          return KeyEventResult.handled;
        }
      }
      return KeyEventResult.ignored;
    }

    return Container(
      width: double.infinity,
      constraints: BoxConstraints(
        maxWidth: conversationId == "" ? 600 : double.infinity,
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Container(
            width: double.infinity,
            child: Row(
              children: [
                for (var attachment in attachments ?? [])
                  AttachmentViewer(
                    attachment: attachment,
                    removeAttachment: removeAttachment,
                    editMode: true,
                  ),
              ],
            ),
          ),
          // if (isMobile)
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
                        controller: messageController,
                        focusNode: inputFocusNode,
                        decoration: InputDecoration(
                          hintText: 'Message...',
                          border: null, // OutlineInputBorder(),
                        ),
                        maxLines: null,
                        minLines: 1,
                        autofocus: conversationId.isNotEmpty,
                        enabled: !isLoading,
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
                // if (!isMobile) getPlus(),
                IconButton(
                  onPressed:
                      isLoading ? handleStop : () => _handleSubmit(context),
                  icon:
                      isLoading
                          ? Stack(
                            alignment: Alignment.center,
                            children: [
                              SizedBox(
                                width: 24, // Adjust size as needed
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
    ;
  }
}
