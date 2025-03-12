import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:image_picker/image_picker.dart';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';
import './model-picker.dart';
import 'package:speech_to_text/speech_to_text.dart' as stt;
import 'package:flutter/foundation.dart';
import '../utils/types.dart';
import './attachments.dart';
import 'package:desktop_drop/desktop_drop.dart';
import 'package:cross_file/cross_file.dart';
import '../utils/file-types.dart';
import 'package:mime/mime.dart';
import 'package:pasteboard/pasteboard.dart';

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
  bool _isDragging = false;
  final FocusNode _pasteDetectorFocusNode = FocusNode();

  @override
  void initState() {
    super.initState();
    _speech = stt.SpeechToText();
    widget.messageController.addListener(_onTextChanged);
  }

  @override
  void dispose() {
    _pasteDetectorFocusNode.dispose();
    super.dispose();
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

  // Process clipboard paste event
  Future<void> _handlePaste(BuildContext context) async {
    // Check for image in clipboard
    try {
      final imageBytes = await Pasteboard.image;
      if (imageBytes != null) {
        await _processPastedImage(imageBytes);
        return;
      }
    } catch (e) {
      print('Error checking for clipboard image: $e');
    }

    // Check for files in clipboard
    try {
      final files = await Pasteboard.files();
      if (files.isNotEmpty) {
        for (final file in files) {
          final xFile = XFile(file);
          final String fileName = xFile.name;
          final String fileExt = fileName.split('.').last.toLowerCase();

          if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp'].contains(fileExt)) {
            await _processDroppedImage(xFile);
          } else {
            await _processDroppedFile(xFile, fileExt);
          }
        }
        return;
      }
    } catch (e) {
      print('Error checking for clipboard files: $e');
    }

    // paste to the field
    try {
      final text = await Pasteboard.text;
      if (text != null) {
        widget.messageController.text += text;
      }
    } catch (e) {
      print('Error checking for clipboard text: $e');
    }
  }

  // Process pasted image data
  Future<void> _processPastedImage(Uint8List imageBytes) async {
    try {
      final base64Data = base64Encode(imageBytes);

      // Try to detect the mime type from the image bytes
      final mimeType =
          lookupMimeType('', headerBytes: imageBytes) ?? 'image/png';

      setState(() {
        // Remove existing image attachments because we only support one image for now
        widget.attachments.removeWhere((a) => a.type == 'image_url');
        widget.addAttachment(
          Attachment(
            type: 'image_url',
            content: 'data:$mimeType;base64,$base64Data',
          ),
        );
      });

      // Show confirmation message
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Image pasted from clipboard'),
          duration: Duration(seconds: 2),
          showCloseIcon: true,
        ),
      );
    } catch (e) {
      print('Error processing pasted image: $e');
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Failed to process pasted image'),
          showCloseIcon: true,
        ),
      );
    }
  }

  // Process dropped files
  Future<void> _handleDroppedFiles(List<XFile> files) async {
    for (final file in files) {
      final String fileName = file.name;
      final String fileExt = fileName.split('.').last.toLowerCase();

      // Check if it's an image file
      if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp'].contains(fileExt)) {
        await _processDroppedImage(file);
      } else {
        await _processDroppedFile(file, fileExt);
      }
    }
  }

  // Process dropped images
  Future<void> _processDroppedImage(XFile file) async {
    try {
      final bytes = await file.readAsBytes();
      final mimeType = _getMimeType(file.name) ?? 'image/jpeg';
      final base64Data = base64Encode(bytes);

      setState(() {
        // Remove existing image attachments because we only support one image for now
        widget.attachments.removeWhere((a) => a.type == 'image_url');
        widget.addAttachment(
          Attachment(
            type: 'image_url',
            content: 'data:$mimeType;base64,$base64Data',
          ),
        );
      });
    } catch (e) {
      print('Error processing dropped image: $e');
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Failed to process image'),
          showCloseIcon: true,
        ),
      );
    }
  }

  // Process dropped files
  Future<void> _processDroppedFile(XFile file, String fileExt) async {
    try {
      if (textFileExtensions.contains(fileExt)) {
        final bytes = await file.readAsBytes();
        final text = utf8.decode(bytes);
        widget.addAttachment(
          Attachment(
            type: 'snippet',
            filename: file.name,
            ext: fileExt,
            content: text,
          ),
        );
      } else if (documentFileExtensions.contains(fileExt)) {
        final bytes = await file.readAsBytes();
        final base64Data = base64Encode(bytes);
        final mimeType = _getMimeType(file.name) ?? 'application/octet-stream';

        widget.addAttachment(
          Attachment(
            type: 'file',
            filename: file.name,
            ext: fileExt,
            content: 'data:$mimeType;base64,$base64Data',
          ),
        );
      } else {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text('Unsupported file type: .$fileExt'),
            showCloseIcon: true,
          ),
        );
      }
    } catch (e) {
      print('Error processing dropped file: $e');
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Failed to process file'),
          showCloseIcon: true,
        ),
      );
    }
  }

  // Helper to get MIME type from file extension
  String? _getMimeType(String fileName) {
    final ext = fileName.split('.').last.toLowerCase();
    final mimeTypes = {
      'jpg': 'image/jpeg',
      'jpeg': 'image/jpeg',
      'png': 'image/png',
      'gif': 'image/gif',
      'webp': 'image/webp',
      'bmp': 'image/bmp',
      'txt': 'text/plain',
      'md': 'text/markdown',
      'pdf': 'application/pdf',
      'doc': 'application/msword',
      'docx':
          'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    };
    return mimeTypes[ext];
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
    // Handle keyboard shortcuts
    if (event is KeyDownEvent) {
      // Enter key handling
      if (event.logicalKey == LogicalKeyboardKey.enter) {
        if (HardwareKeyboard.instance.isShiftPressed) {
          return KeyEventResult.ignored;
        } else {
          _handleSubmit(context);
          return KeyEventResult.handled;
        }
      }

      // Paste handling with Ctrl+V or Cmd+V
      if ((event.logicalKey == LogicalKeyboardKey.keyV) &&
          (HardwareKeyboard.instance.isControlPressed ||
              HardwareKeyboard.instance.isMetaPressed)) {
        _handlePaste(context);
        // We return ignored to let the default paste behavior also happen
        return KeyEventResult.ignored;
      }
    }
    return KeyEventResult.ignored;
  }

  @override
  Widget build(BuildContext context) {
    bool supportSST =
        kIsWeb || Platform.isAndroid || Platform.isIOS || Platform.isMacOS;

    // Wrap with DropTarget for drag & drop functionality
    return DropTarget(
      onDragDone: (detail) {
        _handleDroppedFiles(detail.files);
      },
      onDragEntered: (detail) {
        setState(() {
          _isDragging = true;
        });
      },
      onDragExited: (detail) {
        setState(() {
          _isDragging = false;
        });
      },
      child: Container(
        width: double.infinity,
        constraints: BoxConstraints(
          maxWidth: widget.conversationId == "" ? 600 : double.infinity,
        ),
        decoration: BoxDecoration(
          border:
              _isDragging
                  ? Border.all(color: Color(0xffee4654), width: 2.0)
                  : null,
          borderRadius: _isDragging ? BorderRadius.circular(4.0) : null,
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            if (_isDragging)
              Container(
                width: double.infinity,
                padding: EdgeInsets.all(16.0),
                alignment: Alignment.center,
                child: Text(
                  'Drop files or images here',
                  style: TextStyle(
                    color: Color(0xffee4654),
                    fontWeight: FontWeight.bold,
                  ),
                ),
              ),
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
                        child: CallbackShortcuts(
                          bindings: {
                            const SingleActivator(
                                  LogicalKeyboardKey.keyV,
                                  control: true,
                                ):
                                () => _handlePaste(context),
                            const SingleActivator(
                                  LogicalKeyboardKey.keyV,
                                  meta: true,
                                ):
                                () => _handlePaste(context),
                          },
                          child: TextFormField(
                            controller: widget.messageController,
                            focusNode: widget.inputFocusNode,
                            decoration: InputDecoration(
                              hintText:
                                  _isDragging
                                      ? 'Drop files here...'
                                      : 'Your message...',
                              border: null, // OutlineInputBorder(),
                            ),
                            maxLines: null,
                            minLines: 1,
                            autofocus: widget.conversationId.isNotEmpty,
                            enabled: !widget.isLoading,
                            keyboardType: TextInputType.multiline,
                            textInputAction: TextInputAction.newline,
                            validator: (value) {
                              if ((value == null || value.trim().isEmpty) &&
                                  widget.attachments.isEmpty) {
                                return 'Please enter a message or add an attachment';
                              }
                              return null;
                            },
                          ),
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
                  // Paste button for explicit paste functionality
                  // IconButton(
                  //   onPressed: () => _handlePaste(context),
                  //   icon: const Icon(Icons.content_paste, color: Color(0xffee4654)),
                  //   tooltip: 'Paste from clipboard',
                  // ),
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
                                const Icon(
                                  Icons.stop,
                                  color: Color(0xffee4654),
                                ),
                              ],
                            )
                            : const Icon(Icons.send, color: Color(0xffee4654)),
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
