import 'dart:convert';
import 'package:hive_ce/hive.dart';
import 'package:plurality/api/mini-apps.dart';
import 'package:plurality/chat/message-list/model-picker.dart';
part 'types.g.dart';

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

@HiveType(typeId: 2)
class ContentPart {
  @HiveField(0)
  final String type; // "text" or "image_url"

  @HiveField(1)
  final String? text;

  @HiveField(2)
  final ContentImageURL? imageUrl;

  ContentPart({required this.type, this.text, this.imageUrl});

  Map<String, dynamic> toJson() {
    final data = <String, dynamic>{'type': type};
    if (type == 'text' && text != null) data['text'] = text;
    if (type == 'image_url' && imageUrl != null) {
      data['image_url'] = imageUrl!.toJson();
    }
    return data;
  }

  factory ContentPart.fromJson(Map<String, dynamic> json) {
    return ContentPart(
      type: json['type'] ?? 'text',
      text: json['text'],
      imageUrl:
          json['image_url'] != null
              ? ContentImageURL.fromJson(json['image_url'])
              : null,
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

  Message({
    required this.role,
    required this.content,
    DateTime? timestamp,
    this.totalTokens,
    this.model,
    this.toolCalls,
    this.toolCallId,
    this.name,
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
    if (content.length == 1 && content[0].type == 'text') {
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
      parsedContent =
          rawContent
              .map((c) => ContentPart.fromJson(c as Map<String, dynamic>))
              .toList();
    }

    // Parse tool_calls
    List<ToolCall>? toolCalls;
    if (json['tool_calls'] != null) {
      toolCalls =
          (json['tool_calls'] as List)
              .map((t) => ToolCall.fromJson(t as Map<String, dynamic>))
              .toList();
    }

    return Message(
      role: json['role'] ?? 'user',
      content: parsedContent,
      toolCalls: toolCalls,
      toolCallId: json['tool_call_id'],
      name: json['name'],
      totalTokens: json['total_tokens'],
      model: json['model'] != null ? Model.fromJson(json['model']) : null,
      timestamp:
          json['timestamp'] != null
              ? DateTime.parse(json['timestamp'])
              : null,
    );
  }
}

// --- Conversation State ---

enum ConversationState { idle, processing, waitingForTool }

ConversationState conversationStateFromString(String? state) {
  switch (state) {
    case 'processing':
      return ConversationState.processing;
    case 'waiting_for_tool':
      return ConversationState.waitingForTool;
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

  ConversationState get state => conversationStateFromString(stateString);
  set state(ConversationState value) => stateString = value.name;

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
          json['messages'] != null
              ? (json['messages'] as List)
                  .map((m) => Message.fromJson(m as Map<String, dynamic>))
                  .toList()
              : [],
      modelSelected:
          json['model_selected'] != null
              ? ModelSelected.fromJson(json['model_selected'])
              : ModelSelected(),
      icon: json['icon'],
      stateString: json['state'] as String?,
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

  Attachment({
    required this.type,
    required this.content,
    this.filename,
    this.ext,
  });

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

  const ModelSelected({
    this.text = modelPresentFastText,
    this.vision = modelPresentFastVision,
    this.imageGen = modelPresentFastImageGen,
    this.audioGen = const Model(
      name: 'cartesia/sonic',
      params: {"voice": "sweet lady"},
    ),
    this.voiceGen = const Model(name: '', params: {}),
    this.audioTranscribe = const Model(name: '', params: {}),
    this.videoGen = const Model(name: '', params: {}),
    this.videoVision = const Model(name: '', params: {}),
    this.code = const Model(
      name: 'codellama/CodeLlama-34b-Instruct-hf',
      params: {},
    ),
  });

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
    );
  }
}

@HiveType(typeId: 7)
class Model {
  @HiveField(0)
  final String name;

  @HiveField(1)
  final Map<String, String>? params;

  @HiveField(2)
  final List<String> tools;

  const Model({required this.name, required this.params, this.tools = const []});

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

    List<String> tools = [];
    try {
      if (json['tools'] != null) {
        tools = List<String>.from(json['tools']);
      }
    } catch (_) {
      tools = [];
    }

    return Model(name: (json['name'] ?? '') as String, params: params, tools: tools);
  }
}

// --- App Preferences ---

class AppPreferences {
  final ModelSelected selectedModel;
  final int darkMode;
  final bool useMiniMap;

  const AppPreferences({
    this.selectedModel = const ModelSelected(),
    this.darkMode = 0,
    this.useMiniMap = true,
  });

  AppPreferences copyWith({
    ModelSelected? selectedModel,
    int? darkMode,
    bool? useMiniMap,
  }) {
    return AppPreferences(
      selectedModel: selectedModel ?? this.selectedModel,
      darkMode: darkMode ?? this.darkMode,
      useMiniMap: useMiniMap ?? this.useMiniMap,
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
