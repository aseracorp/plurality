// MessageContentURL to match the backend structure
class MessageContentURL {
  final String url;

  MessageContentURL({required this.url});

  Map<String, dynamic> toJson() => {'url': url};

  factory MessageContentURL.fromJson(Map<String, dynamic> json) {
    return MessageContentURL(url: json['url']);
  }
}

// MessageContent to represent different content types
class MessageContent {
  final String type;
  String? text;
  MessageContentURL? imageUrl;

  MessageContent({required this.type, this.text, this.imageUrl});

  Map<String, dynamic> toJson() {
    final Map<String, dynamic> data = {'type': type};

    if (text != null) {
      data['text'] = text;
    }

    if (imageUrl != null) {
      data['image_url'] = imageUrl!.toJson();
    }

    return data;
  }

  factory MessageContent.fromJson(Map<String, dynamic> json) {
    return MessageContent(
      type: json['type'],
      text: json['text'],
      imageUrl:
          json['image_url'] != null
              ? MessageContentURL.fromJson(json['image_url'])
              : null,
    );
  }

  // Helper constructor for text content
  factory MessageContent.text(String text) {
    return MessageContent(type: 'text', text: text);
  }

  // Helper constructor for image content
  factory MessageContent.image(String imageUrl) {
    return MessageContent(
      type: 'image_url',
      imageUrl: MessageContentURL(url: imageUrl),
    );
  }
}

// Refactored Message class
class Message {
  final String role;
  final List<MessageContent> content;
  DateTime timestamp;

  Message({required this.role, required this.content, DateTime? timestamp})
    : timestamp = timestamp ?? DateTime.now();

  bool get isBot => role == 'assistant';

  // Create a message with text content only
  factory Message.text({required String text, required String role}) {
    return Message(role: role, content: [MessageContent.text(text)]);
  }

  // Create a message with both image and text
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

  // Convert to JSON for local storage
  Map<String, dynamic> toJson() => {
    'role': role,
    'timestamp': timestamp.toIso8601String(), // Convert DateTime to string
    'content': content.map((c) => c.toJson()).toList(),
  };

  // Convert to API format (if needed for backward compatibility)
  Map<String, dynamic> toAPI() => {
    'role': role,
    'content': content.map((c) => c.toJson()).toList(),
  };

  // Create from JSON (local storage format)
  factory Message.fromJson(Map<String, dynamic> json) {
    return Message(
      role: json['role'],
      timestamp:
          json['timestamp'] != null ? DateTime.parse(json['timestamp']) : null,
      content:
          (json['content'] as List)
              .map((c) => MessageContent.fromJson(c as Map<String, dynamic>))
              .toList(),
    );
  }

  // Helper method to get text content
  String get text {
    final textContent = content.firstWhere(
      (element) => element.type == 'text',
      orElse: () => MessageContent.text(''),
    );
    return textContent.text ?? '';
  }

  // Helper method to get image URL if exists
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

// Refactored Conversation class
class Conversation {
  final String id;
  final String userId;
  final String title;
  final DateTime createdAt;
  final DateTime lastMessageAt;
  final List<Message> messages;

  Conversation({
    required this.id,
    required this.userId,
    required this.title,
    required this.lastMessageAt,
    DateTime? createdAt,
    required this.messages,
  }) : createdAt = createdAt ?? DateTime.now();

  Map<String, dynamic> toJson() => {
    'id': id,
    'user_id': userId,
    'title': title,
    'created_at': createdAt.toIso8601String(),
    'last_message_at': lastMessageAt.toIso8601String(),
    'messages': messages.map((m) => m.toJson()).toList(),
  };

  Map<String, dynamic> toAPI() => {
    'id': id,
    'user_id': userId,
    'title': title,
    'last_message_at': lastMessageAt.toIso8601String(),
    'messages': messages.map((m) => m.toAPI()).toList(),
  };

  factory Conversation.fromJson(Map<String, dynamic> json) {
    return Conversation(
      id: json['id'] ?? json['_id'] ?? '',
      userId: json['user_id'] ?? '',
      title: json['title'] ?? 'Untitled',
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
    );
  }
}

// Custom exception for API errors
class APIException implements Exception {
  final String message;
  final int? statusCode;

  APIException(this.message, {this.statusCode});

  @override
  String toString() => message;
}
