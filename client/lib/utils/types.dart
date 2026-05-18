import 'dart:convert';
import 'package:hive_ce/hive.dart';
import 'package:plurality/api/mini-apps.dart';
part 'types.g.dart';

// --- Helpers ---

/// Convert a dynamic value to Map<String, dynamic>, handling both normal JSON
/// objects and BSON primitive.D format ([{Key: k, Value: v}, ...]) that the
/// Go MongoDB driver can produce when serializing interface{} fields.
Map<String, dynamic>? _toMapOrNull(dynamic value) {
  if (value is Map<String, dynamic>) return value;
  if (value is Map) return value.cast<String, dynamic>();
  if (value is List) {
    // BSON primitive.D: [{Key: "type", Value: "text"}, ...]
    final map = <String, dynamic>{};
    for (final pair in value) {
      if (pair is Map && pair.containsKey('Key') && pair.containsKey('Value')) {
        final v = pair['Value'];
        map[pair['Key'].toString()] = v is List ? (_toMapOrNull(v) ?? v) : v;
      }
    }
    if (map.isNotEmpty) return map;
  }
  return null;
}

// --- OpenAI-Compatible Message Types ---

@HiveType(typeId: 1)
class ContentImageURL {
  @HiveField(0)
  final String url;

  ContentImageURL({required this.url});

  Map<String, dynamic> toJson() => {'url': url};

  factory ContentImageURL.fromJson(Map<String, dynamic> json) {
    return ContentImageURL(url: json['url'] ?? '');
  }
}

const _documentTypes = {'pdf', 'docx', 'xlsx', 'pptx'};

@HiveType(typeId: 2)
class ContentPart {
  @HiveField(0)
  final String type; // "text", "image_url", "snippet", "file", "pdf", "docx", "xlsx", "pptx"

  @HiveField(1)
  final String? text;

  @HiveField(2)
  final ContentImageURL? imageUrl;

  @HiveField(3)
  final String? filename;

  ContentPart({required this.type, this.text, this.imageUrl, this.filename});

  /// True for any content part that carries text (text, snippet, file, etc.).
  /// image_url and document parts are non-text.
  bool get isTextLike => type != 'image_url' && !_documentTypes.contains(type);

  Map<String, dynamic> toJson() {
    final data = <String, dynamic>{'type': type};
    if (text != null && text!.isNotEmpty) data['text'] = text;
    if (!isTextLike && imageUrl != null) data['image_url'] = imageUrl!.toJson();
    if (filename != null && filename!.isNotEmpty) data['filename'] = filename;
    return data;
  }

  factory ContentPart.fromJson(Map<String, dynamic> json) {
    ContentImageURL? imageUrl;
    if (json['image_url'] != null) {
      final imgMap = _toMapOrNull(json['image_url']);
      if (imgMap != null) {
        imageUrl = ContentImageURL.fromJson(imgMap);
      }
    }
    return ContentPart(
      type: json['type'] ?? 'text',
      text: json['text'],
      imageUrl: imageUrl,
      filename: json['filename'],
    );
  }
}

@HiveType(typeId: 11)
class FunctionCallData {
  @HiveField(0)
  final String name;

  @HiveField(1)
  final String arguments;

  FunctionCallData({required this.name, required this.arguments});

  Map<String, dynamic> toJson() => {'name': name, 'arguments': arguments};

  factory FunctionCallData.fromJson(Map<String, dynamic> json) {
    return FunctionCallData(
      name: json['name'] ?? '',
      arguments: json['arguments'] ?? '',
    );
  }
}

@HiveType(typeId: 10)
class ToolCall {
  @HiveField(0)
  final String id;

  @HiveField(1)
  final String type; // "function"

  @HiveField(2)
  final FunctionCallData function;

  @HiveField(3)
  final String loading;  // display template, e.g. "Search for \"{{query}}\""

  @HiveField(4)
  final String iconURL;  // base64 icon

  ToolCall({
    required this.id,
    this.type = 'function',
    required this.function,
    this.loading = '',
    this.iconURL = '',
  });

  Map<String, dynamic> toJson() {
    final data = <String, dynamic>{
      'id': id,
      'type': type,
      'function': function.toJson(),
    };
    if (loading.isNotEmpty) data['loading'] = loading;
    if (iconURL.isNotEmpty) data['icon_url'] = iconURL;
    return data;
  }

  factory ToolCall.fromJson(Map<String, dynamic> json) {
    if (json.containsKey('function')) {
      return ToolCall(
        id: json['id'] ?? '',
        type: json['type'] ?? 'function',
        function: FunctionCallData.fromJson(json['function']),
        loading: json['loading'] ?? '',
        iconURL: json['icon_url'] ?? '',
      );
    }
    // Legacy fallback
    return ToolCall(
      id: json['id'] ?? json['tool_use_id'] ?? '',
      type: 'function',
      function: FunctionCallData(
        name: json['name'] ?? '',
        arguments: json['arguments'] ?? '',
      ),
      loading: json['loading'] ?? '',
      iconURL: json['icon_url'] ?? '',
    );
  }
}

// --- Message ---

@HiveType(typeId: 3)
class Message {
  @HiveField(0)
  final String role; // "user", "assistant", "tool"

  @HiveField(1)
  final List<ContentPart> content; // Internally always a list

  @HiveField(2)
  DateTime timestamp;

  @HiveField(3)
  int? totalTokens;

  @HiveField(4)
  Model? model;

  @HiveField(5)
  List<ToolCall>? toolCalls; // Assistant messages only

  @HiveField(6)
  String? toolCallId; // Tool messages only

  @HiveField(7)
  String? name; // Tool messages only

  @HiveField(8)
  int? promptTokens;

  @HiveField(9)
  int? completionTokens;

  @HiveField(10)
  double? responseCost;

  Message({
    required this.role,
    required this.content,
    DateTime? timestamp,
    this.totalTokens,
    this.model,
    this.toolCalls,
    this.toolCallId,
    this.name,
    this.promptTokens,
    this.completionTokens,
    this.responseCost,
  }) : timestamp = timestamp ?? DateTime.now();

  bool get isBot => role == 'assistant';
  bool get isToolResult => role == 'tool';
  bool get hasToolCalls =>
      toolCalls != null && toolCalls!.isNotEmpty;

  /// Extract the first text content from the message.
  String get textContent {
    for (final part in content) {
      if (part.type == 'text' && part.text != null && part.text!.isNotEmpty) {
        return part.text!;
      }
    }
    return '';
  }

  /// Check if the message contains any images.
  bool get hasImages => content.any((p) => p.type == 'image_url');

  /// Get all image URLs from the message.
  List<String> get imageUrls => content
      .where((p) => p.type == 'image_url' && p.imageUrl != null)
      .map((p) => p.imageUrl!.url)
      .toList();

  // --- Constructors ---

  factory Message.text({required String text, required String role}) {
    return Message(
      role: role,
      content: [ContentPart(type: 'text', text: text)],
    );
  }

  factory Message.withImages({
    required String text,
    required String role,
    required List<String> imageUrls,
  }) {
    return Message(
      role: role,
      content: [
        ...imageUrls.map(
          (url) => ContentPart(
            type: 'image_url',
            imageUrl: ContentImageURL(url: url),
          ),
        ),
        ContentPart(type: 'text', text: text),
      ],
    );
  }

  factory Message.toolResult({
    required String toolCallId,
    required String name,
    required String result,
  }) {
    return Message(
      role: 'tool',
      toolCallId: toolCallId,
      name: name,
      content: [ContentPart(type: 'text', text: result)],
    );
  }

  // --- Serialization ---

  /// Serialize for the server API (OpenAI format).
  /// Content is a string if it's simple text, array otherwise.
  Map<String, dynamic> toJson() {
    final data = <String, dynamic>{'role': role};

    // Content: string for simple text, array for multi-part
    if (content.length == 1 && content[0].isTextLike) {
      data['content'] = content[0].text ?? '';
    } else if (content.isNotEmpty) {
      data['content'] = content.map((c) => c.toJson()).toList();
    }

    if (toolCalls != null && toolCalls!.isNotEmpty) {
      data['tool_calls'] = toolCalls!.map((t) => t.toJson()).toList();
    }
    if (toolCallId != null) data['tool_call_id'] = toolCallId;
    if (name != null) data['name'] = name;
    if (totalTokens != null) data['total_tokens'] = totalTokens;
    if (promptTokens != null) data['prompt_tokens'] = promptTokens;
    if (completionTokens != null) data['completion_tokens'] = completionTokens;
    if (responseCost != null) data['response_cost'] = responseCost;
    if (model != null) data['model'] = model!.toJson();
    data['timestamp'] = timestamp.toIso8601String();

    return data;
  }

  factory Message.fromJson(Map<String, dynamic> json) {
    // Parse content: can be string, array, or null
    List<ContentPart> parsedContent = [];
    final rawContent = json['content'];
    if (rawContent is String) {
      if (rawContent.isNotEmpty) {
        parsedContent = [ContentPart(type: 'text', text: rawContent)];
      }
    } else if (rawContent is List) {
      for (final c in rawContent) {
        final map = _toMapOrNull(c);
        if (map != null) {
          parsedContent.add(ContentPart.fromJson(map));
        }
      }
    }

    // Parse tool_calls
    List<ToolCall>? toolCalls;
    if (json['tool_calls'] != null && json['tool_calls'] is List) {
      for (final t in json['tool_calls'] as List) {
        final map = _toMapOrNull(t);
        if (map != null) {
          toolCalls ??= [];
          toolCalls.add(ToolCall.fromJson(map));
        }
      }
    }

    return Message(
      role: json['role'] ?? 'user',
      content: parsedContent,
      toolCalls: toolCalls,
      toolCallId: json['tool_call_id'],
      name: json['name'],
      totalTokens: json['total_tokens'],
      promptTokens: json['prompt_tokens'],
      completionTokens: json['completion_tokens'],
      responseCost: (json['response_cost'] as num?)?.toDouble(),
      model: json['model'] != null ? Model.fromJson(json['model']) : null,
      timestamp:
          json['timestamp'] != null
              ? DateTime.parse(json['timestamp'])
              : null,
    );
  }
}

// --- Conversation State ---

enum ConversationState { idle, processing, waitingForTool, waitingForApproval }

ConversationState conversationStateFromString(String? state) {
  switch (state) {
    case 'processing':
      return ConversationState.processing;
    case 'waiting_for_tool':
      return ConversationState.waitingForTool;
    case 'waiting_for_approval':
      return ConversationState.waitingForApproval;
    default:
      return ConversationState.idle;
  }
}

// --- Conversation ---

@HiveType(typeId: 4)
class Conversation extends HiveObject {
  @HiveField(0)
  final String id;

  @HiveField(2)
  String title;

  @HiveField(3)
  final DateTime createdAt;

  @HiveField(4)
  DateTime lastMessageAt;

  @HiveField(5)
  List<Message> messages;

  @HiveField(6)
  ModelSelected modelSelected;

  @HiveField(7)
  MiniApp? miniApp;

  @HiveField(8)
  String? folder;

  @HiveField(9)
  String? icon;

  @HiveField(10)
  String stateString;

  @HiveField(11)
  String? triggerId;

  @HiveField(12)
  String? triggerType;

  ConversationState get state => conversationStateFromString(stateString);
  set state(ConversationState value) => stateString = value.name;

  bool get isTriggered => triggerType != null && triggerType!.isNotEmpty;

  // Triggered conversations are hidden by default, but stay visible while
  // active (processing, waiting on a tool, or waiting on user approval).
  // "conversation"-type triggers are user-initiated sub-conversations and
  // are never hidden.
  bool get isHidden =>
      isTriggered &&
      triggerType != 'conversation' &&
      state == ConversationState.idle;

  Conversation({
    required this.id,
    required this.title,
    required this.lastMessageAt,
    required this.modelSelected,
    DateTime? createdAt,
    required this.messages,
    this.miniApp,
    this.folder,
    this.icon,
    String? stateString,
    this.triggerId,
    this.triggerType,
  }) : createdAt = createdAt ?? DateTime.now(),
       stateString = stateString ?? 'idle';

  Map<String, dynamic> toJson() => {
    'id': id,
    'title': title,
    'created_at': createdAt.toIso8601String(),
    'last_message_at': lastMessageAt.toIso8601String(),
    'messages': messages.map((m) => m.toJson()).toList(),
    'model_selected': modelSelected.toJson(),
    'mini_app': miniApp?.toJson(),
    'folder': folder,
    'icon': icon,
    'state': state.name,
    'trigger_id': triggerId,
    'trigger_type': triggerType,
  };

  factory Conversation.fromJson(Map<String, dynamic> json) {
    return Conversation(
      id: json['id'] ?? json['_id'] ?? '',
      title: json['title'] ?? 'Untitled',
      miniApp:
          json['mini_app'] != null ? MiniApp.fromJson(json['mini_app']) : null,
      folder: json['folder'],
      createdAt:
          json['created_at'] != null
              ? DateTime.parse(json['created_at'])
              : DateTime.now(),
      lastMessageAt:
          json['last_message_at'] != null
              ? DateTime.parse(json['last_message_at'])
              : DateTime.now(),
      messages:
          json['messages'] != null && json['messages'] is List
              ? (json['messages'] as List)
                  .map((m) => _toMapOrNull(m))
                  .where((m) => m != null)
                  .map((m) => Message.fromJson(m!))
                  .toList()
              : [],
      modelSelected:
          json['model_selected'] != null
              ? ModelSelected.fromJson(json['model_selected'])
              : ModelSelected(),
      icon: json['icon'],
      stateString: json['state'] as String?,
      triggerId: json['trigger_id'] as String?,
      triggerType: json['trigger_type'] as String?,
    );
  }
}

// --- Exceptions ---

class APIException implements Exception {
  final String message;
  final int? statusCode;

  APIException(this.message, {this.statusCode});

  @override
  String toString() => message;
}

// --- Upload (POST /upload response) ---

class UploadResult {
  final String url;
  final String filename;
  final String ext;
  final String type;
  final int size;

  UploadResult({
    required this.url,
    required this.filename,
    required this.ext,
    required this.type,
    required this.size,
  });

  factory UploadResult.fromJson(Map<String, dynamic> json) {
    return UploadResult(
      url: json['url'] as String,
      filename: json['filename'] as String? ?? '',
      ext: json['ext'] as String? ?? '',
      type: json['type'] as String? ?? 'file',
      size: (json['size'] as num?)?.toInt() ?? 0,
    );
  }
}

// --- Attachments (for input UI) ---

@HiveType(typeId: 5)
class Attachment {
  @HiveField(0)
  final String type;

  @HiveField(1)
  final String content;

  @HiveField(2)
  final String? filename;

  @HiveField(3)
  final String? ext;

  // Transient UI state. Not persisted to Hive, not sent in toJson.
  final bool uploading;
  final String? uploadError;

  Attachment({
    required this.type,
    required this.content,
    this.filename,
    this.ext,
    this.uploading = false,
    this.uploadError,
  });

  Attachment copyWith({
    String? type,
    String? content,
    String? filename,
    String? ext,
    bool? uploading,
    String? uploadError,
    bool clearUploadError = false,
  }) {
    return Attachment(
      type: type ?? this.type,
      content: content ?? this.content,
      filename: filename ?? this.filename,
      ext: ext ?? this.ext,
      uploading: uploading ?? this.uploading,
      uploadError: clearUploadError ? null : (uploadError ?? this.uploadError),
    );
  }

  Map<String, dynamic> toJson() => {
    'type': type,
    'content': content,
    'filename': filename,
    'ext': ext,
  };

  factory Attachment.fromJson(Map<String, dynamic> json) {
    return Attachment(
      type: json['type'],
      content: json['content'],
      filename: json['filename'],
      ext: json['ext'],
    );
  }
}

// --- Model Configuration ---

@HiveType(typeId: 6)
class ModelSelected {
  @HiveField(0)
  final Model? text;
  @HiveField(1)
  final Model? vision;
  @HiveField(2)
  final Model? imageGen;
  @HiveField(3)
  final Model? audioTranscribe;
  @HiveField(4)
  final Model? voiceGen;
  @HiveField(5)
  final Model? audioGen;
  @HiveField(6)
  final Model? videoGen;
  @HiveField(7)
  final Model? videoVision;
  @HiveField(8)
  final Model? code;

  /// Absolute path of a device-side folder the user has attached to the
  /// conversation. Round-trips with the server alongside the per-conversation
  /// tool toggles — when non-empty, the LLM is given the device-side
  /// filesystem_client tools (sandboxed to this directory) and the client
  /// uses it as the sandbox root when executing those tools.
  @HiveField(9)
  final String? clientFolderPath;

  /// When true, the server compacts older turns into a rolling checkpoint
  /// summary so the live LLM context stays small on long conversations.
  /// Defaults to true; legacy persisted conversations that pre-date the
  /// field also resolve to true (see fromJson).
  @HiveField(10)
  final bool ecoMode;

  const ModelSelected({
    this.text,
    this.vision,
    this.imageGen,
    this.audioGen,
    this.voiceGen,
    this.audioTranscribe,
    this.videoGen,
    this.videoVision,
    this.code,
    this.clientFolderPath,
    this.ecoMode = true,
  });

  ModelSelected copyWith({
    Model? text,
    Model? vision,
    Model? imageGen,
    Model? audioTranscribe,
    Model? voiceGen,
    Model? audioGen,
    Model? videoGen,
    Model? videoVision,
    Model? code,
    Object? clientFolderPath = _unset,
    bool? ecoMode,
  }) {
    return ModelSelected(
      text: text ?? this.text,
      vision: vision ?? this.vision,
      imageGen: imageGen ?? this.imageGen,
      audioTranscribe: audioTranscribe ?? this.audioTranscribe,
      voiceGen: voiceGen ?? this.voiceGen,
      audioGen: audioGen ?? this.audioGen,
      videoGen: videoGen ?? this.videoGen,
      videoVision: videoVision ?? this.videoVision,
      code: code ?? this.code,
      clientFolderPath: identical(clientFolderPath, _unset)
          ? this.clientFolderPath
          : clientFolderPath as String?,
      ecoMode: ecoMode ?? this.ecoMode,
    );
  }

  Map<String, dynamic> toJson() => {
    'text': text?.toJson(),
    'vision': vision?.toJson(),
    'image_gen': imageGen?.toJson(),
    'audio_gen': audioGen?.toJson(),
    'voice_gen': voiceGen?.toJson(),
    'audio_transcribe': audioTranscribe?.toJson(),
    'video_gen': videoGen?.toJson(),
    'video_vision': videoVision?.toJson(),
    'code': code?.toJson(),
    'client_folder_path': clientFolderPath,
    'eco_mode': ecoMode,
  };

  factory ModelSelected.fromJson(Map<String, dynamic> json) {
    return ModelSelected(
      text: json['text'] != null ? Model.fromJson(json['text']) : null,
      vision: json['vision'] != null ? Model.fromJson(json['vision']) : null,
      imageGen:
          json['image_gen'] != null ? Model.fromJson(json['image_gen']) : null,
      audioGen:
          json['audio_gen'] != null ? Model.fromJson(json['audio_gen']) : null,
      voiceGen:
          json['voice_gen'] != null ? Model.fromJson(json['voice_gen']) : null,
      audioTranscribe:
          json['audio_transcribe'] != null
              ? Model.fromJson(json['audio_transcribe'])
              : null,
      videoGen:
          json['video_gen'] != null ? Model.fromJson(json['video_gen']) : null,
      videoVision:
          json['video_vision'] != null
              ? Model.fromJson(json['video_vision'])
              : null,
      code: json['code'] != null ? Model.fromJson(json['code']) : null,
      clientFolderPath: json['client_folder_path'] as String?,
      // Default-on: if the server omits the field (legacy conversations or
      // older builds), treat eco mode as enabled rather than disabled.
      ecoMode: json['eco_mode'] as bool? ?? true,
    );
  }
}

/// Sentinel for [ModelSelected.copyWith] so callers can distinguish "leave
/// unchanged" (default) from "explicitly set to null" (clear the folder).
const Object _unset = Object();

@HiveType(typeId: 7)
class Model {
  @HiveField(0)
  final String name;

  @HiveField(1)
  final Map<String, String>? params;

  @HiveField(2)
  final Map<String, String> tools;

  const Model({required this.name, required this.params, this.tools = const {}});

  Map<String, dynamic> toJson() => {
    'name': name,
    'params': params,
    'tools': tools,
  };

  factory Model.fromJson(Map<String, dynamic> json) {
    Map<String, String>? params;
    try {
      if (json['params'] != null) {
        final rawParams = json['params'] as Map<String, dynamic>;
        params = rawParams.map((key, value) => MapEntry(key, value.toString()));
      }
    } catch (_) {
      params = null;
    }

    Map<String, String> tools = {};
    try {
      if (json['tools'] != null) {
        if (json['tools'] is List) {
          // Backward compat: convert old list format to map
          for (final t in json['tools']) {
            tools[t.toString()] = 'true';
          }
        } else {
          final rawTools = json['tools'] as Map<String, dynamic>;
          tools = rawTools.map((key, value) => MapEntry(key, value.toString()));
        }
      }
    } catch (_) {
      tools = {};
    }

    return Model(name: (json['name'] ?? '') as String, params: params, tools: tools);
  }
}

// --- App Preferences ---

class AppPreferences {
  final ModelSelected selectedModel;
  final int darkMode;
  final bool useMiniMap;
  final double zoomFactor;

  const AppPreferences({
    this.selectedModel = const ModelSelected(),
    this.darkMode = 0,
    this.useMiniMap = true,
    this.zoomFactor = 1.0,
  });

  AppPreferences copyWith({
    ModelSelected? selectedModel,
    int? darkMode,
    bool? useMiniMap,
    double? zoomFactor,
  }) {
    return AppPreferences(
      selectedModel: selectedModel ?? this.selectedModel,
      darkMode: darkMode ?? this.darkMode,
      useMiniMap: useMiniMap ?? this.useMiniMap,
      zoomFactor: zoomFactor ?? this.zoomFactor,
    );
  }
}

// --- MiniApps ---

@HiveType(typeId: 8)
class MiniApp {
  @HiveField(0)
  final String id;

  @HiveField(1)
  final String name;

  @HiveField(2)
  final String description;

  @HiveField(3)
  final String iconURL;

  @HiveField(4)
  final String? author;

  @HiveField(5)
  final ModelSelected? modelSelected;

  @HiveField(6)
  final List<MiniAppInput> inputs;

  @HiveField(7)
  Map<String, String>? initialMessage;

  @HiveField(8)
  final String form;

  @HiveField(9)
  final String placeholder;

  /// "low" / "medium" / "high" — maps to the fast/medium/smart shortcuts when
  /// the preset doesn't pin specific models. Empty defaults to "medium".
  @HiveField(10)
  final String complexity;

  MiniApp({
    required this.id,
    required this.name,
    required this.description,
    required this.iconURL,
    required this.author,
    this.modelSelected,
    required this.inputs,
    this.initialMessage,
    required this.form,
    this.placeholder = '',
    this.complexity = '',
  });

  factory MiniApp.fromJson(Map<String, dynamic> json) {
    var result = MiniApp(
      id: json['id'],
      name: json['name'],
      description: json['description'],
      iconURL: json['icon_url'],
      author: json['author'],
      modelSelected:
          json['model_selected'] != null
              ? ModelSelected.fromJson(json['model_selected'])
              : null,
      inputs:
          json['inputs'] != null
              ? (json['inputs'] as List)
                  .map((input) => MiniAppInput.fromJson(input))
                  .toList()
              : [],
      form: json['form'] ?? '',
      placeholder: json['placeholder'] ?? '',
      complexity: json['complexity'] as String? ?? '',
    );

    try {
      if (json['initial_message'] != null) {
        final rawParams = json['initial_message'] as Map<String, dynamic>;
        result.initialMessage = rawParams.map(
          (key, value) => MapEntry(key, value.toString()),
        );
      }
    } catch (_) {
      result.initialMessage = null;
    }

    return result;
  }

  Map<String, dynamic> toJson() => {
    'id': id,
    'name': name,
    'description': description,
    'icon_url': iconURL,
    'author': author,
    'model_selected': modelSelected?.toJson(),
    'inputs': inputs.map((input) => input.toJson()).toList(),
    'initial_message': initialMessage,
    'form': form,
    'placeholder': placeholder,
    'complexity': complexity,
  };
}

@HiveType(typeId: 9)
class MiniAppInput {
  @HiveField(0)
  final String name;

  @HiveField(1)
  final String description;

  @HiveField(2)
  final String type;

  @HiveField(3)
  final List<String> options;

  MiniAppInput({
    required this.name,
    required this.description,
    required this.type,
    required this.options,
  });

  factory MiniAppInput.fromJson(Map<String, dynamic> json) {
    return MiniAppInput(
      name: json['name'],
      description: json['description'],
      type: json['type'],
      options: List<String>.from(json['options'] ?? []),
    );
  }

  Map<String, dynamic> toJson() => {
    'name': name,
    'description': description,
    'type': type,
    'options': options,
  };
}
