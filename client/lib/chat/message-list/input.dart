import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:image_picker/image_picker.dart';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';
import 'dart:async';
import 'model-picker.dart';
import 'package:speech_to_text/speech_to_text.dart' as stt;
import 'package:flutter/foundation.dart';
import 'attachments.dart';
import 'package:desktop_drop/desktop_drop.dart';
import 'package:cross_file/cross_file.dart';
import 'package:mime/mime.dart';
import 'package:pasteboard/pasteboard.dart';
import '../../utils/types.dart';
import '../../utils/file-types.dart';
import '../../api/stt.dart';
import './model-picker.dart';

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
  final String placeholder;
  final FormFieldValidator<String>? validator;
  final FocusNode inputFocusNode;
  final void Function(Attachment? attachment) removeAttachment;
  final void Function(Attachment attachment) addAttachment;
  final void Function() handleStop;
  final bool isMobile;
  final ModelSelected selectedModel;
  final bool submitButton;
  final bool allowEmptyMessage;

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
    this.placeholder = 'Message...',
    required this.removeAttachment,
    required this.inputFocusNode,
    required this.submitButton,
    required this.allowEmptyMessage,
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
  StreamSubscription? _subscription;

  @override
  void initState() {
    super.initState();
    _speech = stt.SpeechToText();
    widget.messageController.addListener(_onTextChanged);
    if (SpeechRecognitionService().isCall) {
      _call(resuming: true);
    }
  }

  @override
  void dispose() {
    widget.messageController.removeListener(_onTextChanged);
    widget.messageController.dispose();
    _subscription?.cancel();
    _pasteDetectorFocusNode.dispose();
    super.dispose();
  }

  void _onTextChanged() {
    String currentText = widget.messageController.text;

    // Check if text was pasted (significant increase in length)
    if (currentText.length > _previousText.length + 300) {
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

  Future<void> _call({resuming = false}) async {
    print('Call function called');
    final speechService = SpeechRecognitionService();
    if (!resuming)
      await speechService.startRecording(context, autoStop: true, call: true);
    else
      speechService.showRecordingModal(context);

    if (_subscription != null) {
      _subscription!.cancel();
    }

    // Listen to the recording state
    _subscription = speechService.recordingState.listen((isRecording) {
      print('Recording state: $isRecording');

      // When recording stops, you can get the transcribed text
      if (!isRecording) {
        final recognizedText = speechService.recognizedText;
        if (recognizedText.isNotEmpty) {
          // Do something with the transcribed text
          print('Transcribed text: $recognizedText');

          setState(() {
            _currentText = recognizedText;
            // Update the text field with the recognized speech
            widget.messageController.text = _currentText;

            // send the message
            widget.onSend(context);
          });
        } else if (speechService.isCall) {
          _call();
        }
      }
    });
  }

  Future<void> _listen() async {
    // bool supportSST =
    //     kIsWeb || Platform.isAndroid || Platform.isIOS || Platform.isMacOS;

    final speechService = SpeechRecognitionService();
    await speechService.startRecording(context);

    // Listen to the recording state
    speechService.recordingState
        .where(
          (isRecording) => !isRecording,
        ) // Only process when recording stops
        .take(1)
        .listen((isRecording) {
          print('Recording state: $isRecording');

          // When recording stops, you can get the transcribed text
          if (!isRecording) {
            final recognizedText = speechService.recognizedText;
            if (recognizedText.isNotEmpty) {
              // Do something with the transcribed text
              print('Transcribed text: $recognizedText');

              setState(() {
                _currentText = recognizedText;
                // Update the text field with the recognized speech
                widget.messageController.text = _currentText;
              });
            }
          }
        });

    /*if (!_isListening) {
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
    }*/
  }

  String SummarizeSelectedModel(ModelSelected selectedModel) {
    var ValidVisionModels = VisionModelOptions;

    String res = "";
    if (selectedModel.text != null) {
      res += selectedModel.text!.name;
    }

    // if one of the attachments is an image use vision model
    for (var attachment in widget.attachments) {
      if (attachment.type == "image_url") {
        if (selectedModel.vision != null &&
            !ValidVisionModels.contains(selectedModel.text!.name)) {
          res = selectedModel.vision!.name;
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

  Future<void> _handlePaste(BuildContext context) async {
    if (kIsWeb) return;

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

    // paste text to the field at current cursor position
    try {
      final text = await Pasteboard.text;
      if (text != null) {
        // Get current selection
        final TextSelection selection = widget.messageController.selection;
        final String currentText = widget.messageController.text;

        // Calculate new text based on selection
        final String newText;
        final int cursorPosition;

        if (selection.isValid) {
          // Replace selected text or insert at cursor position
          final String beforeSelection = currentText.substring(
            0,
            selection.start,
          );
          final String afterSelection = currentText.substring(selection.end);
          newText = beforeSelection + text + afterSelection;
          cursorPosition = selection.start + text.length;
        } else {
          // If no valid selection, append to end as fallback
          newText = currentText + text;
          cursorPosition = newText.length;
        }

        // Update the controller with the new text and cursor position
        widget.messageController.value = TextEditingValue(
          text: newText,
          selection: TextSelection.collapsed(offset: cursorPosition),
        );
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
        final attType = documentTypeExts.contains(fileExt) ? fileExt : 'file';

        widget.addAttachment(
          Attachment(
            type: attType,
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
      'xls': 'application/vnd.ms-excel',
      'xlsx':
          'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
      'ppt': 'application/vnd.ms-powerpoint',
      'pptx':
          'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    };
    return mimeTypes[ext];
  }

  Widget getPlus(Color primaryColor) {
    return Row(
      children: [
        // Replace the three separate buttons with a single + button
        widget.isMobile
            ? IconButton(
              icon: Icon(Icons.add, color: primaryColor),
              onPressed: () {
                // Show bottom sheet on mobile
                showModalBottomSheet(
                  context: context,
                  builder: (BuildContext context) {
                    return SafeArea(
                      child: Wrap(
                        children: <Widget>[
                          ListTile(
                            leading: Icon(Icons.camera_alt),
                            title: Text('Camera'),
                            onTap: () {
                              Navigator.pop(context);
                              widget.pickImage(source: ImageSource.camera);
                            },
                          ),
                          ListTile(
                            leading: Icon(Icons.photo),
                            title: Text('Gallery'),
                            onTap: () {
                              Navigator.pop(context);
                              widget.pickImage(source: ImageSource.gallery);
                            },
                          ),
                          ListTile(
                            leading: Icon(Icons.attach_file),
                            title: Text('File'),
                            onTap: () {
                              Navigator.pop(context);
                              widget.pickFile();
                            },
                          ),
                        ],
                      ),
                    );
                  },
                );
              },
            )
            : PopupMenuButton<String>(
              icon: Icon(Icons.add, color: primaryColor),
              offset: Offset(0, -100), // Adjust this to position the menu
              itemBuilder:
                  (BuildContext context) => <PopupMenuEntry<String>>[
                    PopupMenuItem<String>(
                      value: 'camera',
                      child: Row(
                        children: [
                          Icon(Icons.camera_alt, color: primaryColor),
                          SizedBox(width: 8),
                          Text('Camera'),
                        ],
                      ),
                    ),
                    PopupMenuItem<String>(
                      value: 'gallery',
                      child: Row(
                        children: [
                          Icon(Icons.photo, color: primaryColor),
                          SizedBox(width: 8),
                          Text('Gallery'),
                        ],
                      ),
                    ),
                    PopupMenuItem<String>(
                      value: 'file',
                      child: Row(
                        children: [
                          Icon(Icons.attach_file, color: primaryColor),
                          SizedBox(width: 8),
                          Text('File'),
                        ],
                      ),
                    ),
                  ],
              onSelected: (String value) {
                switch (value) {
                  case 'camera':
                    widget.pickImage(source: ImageSource.camera);
                    break;
                  case 'gallery':
                    widget.pickImage(source: ImageSource.gallery);
                    break;
                  case 'file':
                    widget.pickFile();
                    break;
                }
              },
            ),

        // Keep the call button separate
        IconButton(
          onPressed: () {
            SpeechRecognitionService().ResetModal();
            _call();
          },
          icon: Icon(Icons.call, color: primaryColor),
        ),

        SizedBox(width: 8),

        // Keep the AI model selection button
        Container(
          constraints: BoxConstraints(maxWidth: 140), // Set maximum width here
          child: ElevatedButton(
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
              mainAxisSize: MainAxisSize.min, // Make row take minimum space
              children: [
                Icon(Icons.smart_toy, size: 16),
                SizedBox(width: 4),
                Flexible(
                  child: Text(
                    SummarizeSelectedModel(widget.selectedModel),
                    style: TextStyle(fontSize: 10),
                    overflow:
                        TextOverflow.ellipsis, // Add ellipsis for text overflow
                    maxLines: 1,
                  ),
                ),
              ],
            ),
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

      if (!kIsWeb) {
        // Paste handling with Ctrl+V or Cmd+V
        if ((event.logicalKey == LogicalKeyboardKey.keyV) &&
            (HardwareKeyboard.instance.isControlPressed ||
                HardwareKeyboard.instance.isMetaPressed)) {
          _handlePaste(context);
          // We return ignored to let the default paste behavior also happen
          return KeyEventResult.handled;
        }
      }
    }
    return KeyEventResult.ignored;
  }

  @override
  Widget build(BuildContext context) {
    final primaryColor =
        (Theme.of(context).brightness == Brightness.dark)
            ? Theme.of(context).colorScheme.secondary
            : Theme.of(context).primaryColor;

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
              _isDragging ? Border.all(color: primaryColor!, width: 2.0) : null,
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
                    color: primaryColor,
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
            getPlus(primaryColor!),
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
                          bindings:
                              kIsWeb
                                  ? {}
                                  : {
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
                                      : widget.placeholder,
                              border: null, // OutlineInputBorder(),
                            ),
                            maxLines: null,
                            minLines: 1,
                            autofocus: widget.conversationId.isNotEmpty,
                            enabled: !widget.isLoading,
                            keyboardType: TextInputType.multiline,
                            textInputAction: TextInputAction.newline,
                            validator: (value) {
                              if (widget.allowEmptyMessage) return null;

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
                  // if (supportSST)
                  IconButton(
                    onPressed: _listen,
                    icon: Icon(
                      _isListening ? Icons.mic : Icons.mic_none,
                      color: primaryColor,
                    ),
                  ),
                  // Paste button for explicit paste functionality
                  // IconButton(
                  //   onPressed: () => _handlePaste(context),
                  //   icon: const Icon(Icons.content_paste, color: primaryColor),
                  //   tooltip: 'Paste from clipboard',
                  // ),
                  // Send or Stop button
                  widget.submitButton
                      ? FilledButton.icon(
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
                                      ),
                                    ),
                                    Icon(Icons.stop),
                                  ],
                                )
                                : Icon(Icons.send),
                        label: Text(widget.isLoading ? 'Stop' : 'Go !'),
                      )
                      : IconButton(
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
                                        valueColor:
                                            AlwaysStoppedAnimation<Color>(
                                              primaryColor!,
                                            ),
                                      ),
                                    ),
                                    Icon(Icons.stop, color: primaryColor),
                                  ],
                                )
                                : Icon(Icons.send, color: primaryColor),
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
