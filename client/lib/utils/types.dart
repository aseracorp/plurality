// Step 1: Add Hive annotations to your existing models (minimal changes)
import 'package:hive/hive.dart';
import 'package:plurality/api/mini-apps.dart';
part 'types.g.dart';

@HiveType(typeId: 1)
class MessageContentURL {
  @HiveField(0)
  final String url;

  MessageContentURL({required this.url});

  Map<String, dynamic> toJson() => {'url': url};

  factory MessageContentURL.fromJson(Map<String, dynamic> json) {
    return MessageContentURL(url: json['url']);
  }
}

@HiveType(typeId: 2)
class MessageContent {
  @HiveField(0)
  final String type;

  @HiveField(1)
  String? text;

  @HiveField(2)
  MessageContentURL? imageUrl;

  @HiveField(3)
  ToolCall? toolCall;

  MessageContent({required this.type, this.text, this.imageUrl, this.toolCall});

  Map<String, dynamic> toJson() {
    final Map<String, dynamic> data = {'type': type};

    if (text != null) {
      data['text'] = text;
    }

    if (imageUrl != null) {
      data['image_url'] = imageUrl!.toJson();
    }

    if (toolCall != null) {
      data['tool_call'] = toolCall!.toJson();
    }

    return data;
  }

  factory MessageContent.fromJson(Map<String, dynamic> json) {
    return MessageContent(
      type: json['type'],
      text: json['text'],
      toolCall:
          json['tool_call'] != null
              ? ToolCall.fromJson(json['tool_call'])
              : null,
      imageUrl:
          json['image_url'] != null
              ? MessageContentURL.fromJson(json['image_url'])
              : null,
    );
  }

  factory MessageContent.text(String text) {
    return MessageContent(type: 'text', text: text);
  }

  factory MessageContent.image(String imageUrl) {
    return MessageContent(
      type: 'image_url',
      imageUrl: MessageContentURL(url: imageUrl),
    );
  }

  factory MessageContent.tool(ToolCall tool) {
    if (tool.result != null) {
      return MessageContent(
        type: 'tool_result',
        toolCall: tool,
        text: tool.result,
      );
    } else {
      return MessageContent(type: 'tool_use', toolCall: tool);
    }
  }
}

@HiveType(typeId: 3)
class Message {
  @HiveField(0)
  final String role;

  @HiveField(1)
  final List<MessageContent> content;

  @HiveField(2)
  DateTime timestamp;

  @HiveField(3)
  int? totalTokens;

  @HiveField(4)
  Model? model;

  Message({
    required this.role,
    required this.content,
    DateTime? timestamp,
    int? this.totalTokens,
    Model? this.model,
  }) : timestamp = timestamp ?? DateTime.now();

  bool get isBot => role == 'assistant';

  factory Message.text({required String text, required String role}) {
    return Message(role: role, content: [MessageContent.text(text)]);
  }

  factory Message.withImage({
    required String text,
    required String role,
    required String imageUrl,
  }) {
    return Message(
      role: role,
      content: [MessageContent.image(imageUrl), MessageContent.text(text)],
    );
  }

  Map<String, dynamic> toJson() => {
    'role': role,
    'timestamp': timestamp.toIso8601String(),
    'content': content.map((c) => c.toJson()).toList(),
    'total_tokens': totalTokens,
    'model': model?.toJson(),
  };

  Map<String, dynamic> toAPI() => {
    'role': role,
    'content': content.map((c) => c.toJson()).toList(),
  };

  factory Message.fromJson(Map<String, dynamic> json) {
    return Message(
      role: json['role'],
      totalTokens: json['total_tokens'],
      model: json['model'] != null ? Model.fromJson(json['model']) : null,
      timestamp:
          json['timestamp'] != null ? DateTime.parse(json['timestamp']) : null,
      content:
          (json['content'] as List)
              .map((c) => MessageContent.fromJson(c as Map<String, dynamic>))
              .toList(),
    );
  }

  String get text {
    final textContent = content.firstWhere(
      (element) => element.type == 'text',
      orElse: () => MessageContent.text(''),
    );
    return textContent.text ?? '';
  }

  String get imageUrl {
    try {
      final imageContent = content.firstWhere(
        (element) => element.type == 'image_url',
      );
      return imageContent.imageUrl?.url ?? '';
    } catch (_) {
      return '';
    }
  }
}

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
  }) : createdAt = createdAt ?? DateTime.now();

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
  };

  Map<String, dynamic> toAPI() => {
    'id': id,
    'title': title,
    'last_message_at': lastMessageAt.toIso8601String(),
    'messages': messages.map((m) => m.toAPI()).toList(),
    'model_selected': modelSelected.toJson(),
    'mini_app': miniApp?.toJson(),
    'folder': folder,
    'icon': icon,
  };

  factory Conversation.fromJson(Map<String, dynamic> json) {
    var miniapp =
        json['mini_app'] != null ? MiniApp.fromJson(json['mini_app']) : null;

    return Conversation(
      id: json['id'] ?? json['_id'] ?? '',
      title: json['title'] ?? 'Untitled',
      miniApp: miniapp,
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
    );
  }
}

class APIException implements Exception {
  final String message;
  final int? statusCode;

  APIException(this.message, {this.statusCode});

  @override
  String toString() => message;
}

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
    this.text = const Model(
      name: 'llama-v3p1-70b-instruct',
      params: {},
      tools: ['search_web', 'place_search', 'visit_link'],
    ),
    this.vision = const Model(
      name: 'llama-v3p2-90b-vision-instruct',
      params: {},
      tools: ['search_web', 'place_search', 'visit_link'],
    ),
    this.imageGen = const Model(name: 'flux-1-schnell', params: {}),
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

  const Model({
    required this.name,
    required this.params,
    this.tools = const [],
  });

  Map<String, dynamic> toJson() => {
    'name': name,
    'params': params,
    'tools': tools,
  };

  factory Model.fromJson(Map<String, dynamic> json) {
    Map<String, String>? params;

    try {
      if (json['params'] != null) {
        // Get the original params map
        final rawParams = json['params'] as Map<String, dynamic>;

        // Convert all values to strings
        params = rawParams.map((key, value) => MapEntry(key, value.toString()));
      }
    } catch (e) {
      print('Error parsing params: $e');
      params = null;
    }

    List<String> tools = [];
    try {
      if (json['tools'] != null) {
        tools = List<String>.from(json['tools']);
      }
    } catch (e) {
      print('Error parsing tools: $e');
      tools = [];
    }

    return Model(
      name: (json['name'] ?? '') as String,
      params: params,
      tools: tools,
    );
  }
}

class AppPreferences {
  final ModelSelected selectedModel;
  final int darkMode; // 0 = light, 1 = dark, 2 = system
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
  Map<String, String>? InitialMessage;

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
    this.InitialMessage,
    required this.form,
    this.placeholder = '',
  });

  factory MiniApp.fromJson(Map<String, dynamic> json) {
    print(
      json['model_selected'] != null
          ? ModelSelected.fromJson(json['model_selected']).toJson().toString()
          : null,
    );
    var r = MiniApp(
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
        r.InitialMessage = rawParams.map(
          (key, value) => MapEntry(key, value.toString()),
        );
      }
    } catch (e) {
      print('Error parsing InitialMessage: $e');
      r.InitialMessage = null;
    }

    return r;
  }

  Map<String, dynamic> toJson() {
    return {
      'id': id,
      'name': name,
      'description': description,
      'icon_url': iconURL,
      'author': author,
      'model_selected': modelSelected?.toJson(),
      'inputs': inputs.map((input) => input.toJson()).toList(),
      'InitialMessage': InitialMessage,
      'form': form,
      'placeholder': placeholder,
    };
  }
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

  Map<String, dynamic> toJson() {
    return {
      'name': name,
      'description': description,
      'type': type,
      'options': options,
    };
  }
}

@HiveType(typeId: 10)
class ToolCall {
  @HiveField(0)
  final String id;

  @HiveField(1)
  final String name;

  @HiveField(2)
  final String arguments;

  @HiveField(3)
  final String loading;

  @HiveField(4)
  final String iconURL;

  @HiveField(5)
  final String? result;

  ToolCall({
    required this.id,
    required this.name,
    required this.arguments,
    this.loading = '',
    this.iconURL = '',
    this.result,
  });

  Map<String, dynamic> toJson() => {
    'id': id,
    'name': name,
    'arguments': arguments,
    'loading': loading,
    'icon_url': iconURL,
    'result': result,
  };

  factory ToolCall.fromJson(Map<String, dynamic> json) {
    return ToolCall(
      id: json['id'] ?? json['tool_use_id'] ?? '',
      name: json['name'] ?? '',
      arguments: json['arguments'] ?? '',
      loading: json['loading'] ?? '',
      iconURL: json['icon_url'] ?? '',
      result: json['result'],
    );
  }
}
