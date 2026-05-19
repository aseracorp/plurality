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
import 'package:file_picker/file_picker.dart';
import 'package:path/path.dart' as p;
import 'attachments.dart';
import 'package:desktop_drop/desktop_drop.dart';
import 'package:cross_file/cross_file.dart';
import 'package:mime/mime.dart';
import 'package:pasteboard/pasteboard.dart';
import '../../utils/types.dart';
import '../../utils/file-types.dart';
import '../../api/stt.dart';
import '../../api/models_service.dart';
import '../../api/client_identity.dart';
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
  /// Attach a local file (picker, drop, paste). The handler decides routing:
  /// image → inline, text/code → snippet, otherwise → /upload.
  final Future<void> Function(XFile xFile) attachFile;
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
    required this.attachFile,
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

  bool get _supportsFolderPicker =>
      !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);

  Future<void> _attachFolder() async {
    try {
      final selected = await FilePicker.platform.getDirectoryPath(
        dialogTitle: 'Attach a folder to this conversation',
      );
      if (selected == null || selected.isEmpty) return;
      final canonical = p.canonicalize(selected);
      // Attaching a folder pins this conversation to this client — only
      // this machine can run the device-side filesystem tools. If a lock
      // is already held by us this is a no-op; if held by another client,
      // overwrite (the user explicitly attached the folder here).
      final lock = ClientIdentity().asLock() ?? widget.selectedModel.clientLock;
      widget.setSelectedModel(
        widget.selectedModel.copyWith(
          clientFolderPath: canonical,
          clientLock: lock,
        ),
      );
    } catch (e) {
      debugPrint('[InputBox] folder attach failed: $e');
    }
  }

  void _detachFolder() {
    // Only release the lock for the new-chat (no conversationId) flow:
    // the attach in that flow is what acquired the lock in the first
    // place, so undoing the attach should undo the lock too. For an
    // existing conversation the lock represents ongoing ownership of
    // client-tool execution — possibly acquired by a shell run, not the
    // folder — so a folder detach shouldn't auto-unlock it.
    if (widget.conversationId.isEmpty) {
      widget.setSelectedModel(
        widget.selectedModel.copyWith(
          clientFolderPath: null,
          clientLock: null,
        ),
      );
    } else {
      widget.setSelectedModel(
        widget.selectedModel.copyWith(clientFolderPath: null),
      );
    }
  }

  @override
  void initState() {
    super.initState();
    _speech = stt.SpeechToText();
    widget.messageController.addListener(_onTextChanged);
    // Prime the models cache so SummarizeSelectedModel's vision check has data.
    ModelsService().get().then((_) {
      if (mounted) setState(() {});
    }).catchError((_) {});
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
    final ValidVisionModels = ModelsService().cached?.visionModelIds ?? const <String>[];

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
        for (final filePath in files) {
          await widget.attachFile(XFile(filePath));
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

  /// Hand each dropped/pasted XFile off to the parent's single attach helper.
  Future<void> _attachAllFiles(Iterable<XFile> files) async {
    for (final file in files) {
      try {
        await widget.attachFile(file);
      } catch (e) {
        print('Error attaching file ${file.name}: $e');
        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(
              content: Text('Failed to attach ${file.name}'),
              showCloseIcon: true,
            ),
          );
        }
      }
    }
  }

  /// Small chip rendered next to the model selection button when the
  /// client lock is held by this device. Reminds the user that any
  /// other connected client viewing this conversation is currently in
  /// "read-only" mode for client-side tools (files, shell, MCP). The
  /// trailing X clears the lock so another client can take over —
  /// without forcing them to use the "Move conversation here" banner.
  Widget _lockIndicator(Color primaryColor) {
    final lock = widget.selectedModel.clientLock;
    if (lock == null) return const SizedBox.shrink();
    final myId = ClientIdentity().id;
    if (myId.isEmpty || lock.id != myId) return const SizedBox.shrink();
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 4),
      child: InputChip(
        avatar: Icon(Icons.lock, size: 14, color: primaryColor),
        label: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 120),
          child: Text(
            lock.label,
            style: const TextStyle(fontSize: 10),
            overflow: TextOverflow.ellipsis,
            maxLines: 1,
          ),
        ),
        tooltip:
            "Client tools (files, shell) only run on this device while "
            "the lock is held. Click X to release so another client can take over.",
        onDeleted: () {
          widget.setSelectedModel(
            widget.selectedModel.copyWith(clientLock: null),
          );
        },
        deleteIconColor: primaryColor,
        materialTapTargetSize: MaterialTapTargetSize.shrinkWrap,
        visualDensity: VisualDensity.compact,
      ),
    );
  }

  Widget _folderAttachButton(Color primaryColor) {
    if (!_supportsFolderPicker) {
      return const SizedBox.shrink();
    }
    final attached = widget.selectedModel.clientFolderPath;
    if (attached == null || attached.isEmpty) {
      return IconButton(
        tooltip: 'Attach a folder for the AI to read/write',
        onPressed: _attachFolder,
        icon: Icon(Icons.create_new_folder_outlined, color: primaryColor),
      );
    }
    final name = p.basename(attached);
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 4),
      child: InputChip(
        avatar: Icon(Icons.folder, size: 16, color: primaryColor),
        label: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 120),
          child: Text(
            name,
            overflow: TextOverflow.ellipsis,
            maxLines: 1,
            style: const TextStyle(fontSize: 11),
          ),
        ),
        tooltip: attached,
        onDeleted: _detachFolder,
        deleteIconColor: primaryColor,
        materialTapTargetSize: MaterialTapTargetSize.shrinkWrap,
        visualDensity: VisualDensity.compact,
      ),
    );
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

        // Folder attach button (desktop only) — sandbox for the device-side
        // filesystem tools.
        _folderAttachButton(primaryColor),

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

        _lockIndicator(primaryColor),
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
        _attachAllFiles(detail.files);
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
