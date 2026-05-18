import '../utils/types.dart';

/// SSEEvent matches the server's SSEEvent JSON structure.
/// This is the unified event type received over Server-Sent Events.
class SSEEvent {
  final String type; // "text", "tool_use", "tool_result", "state_change", "done", "error"
  final String? content;
  final ToolCall? toolCall;
  final String? toolCallId;
  final String? toolName;
  final String? toolResult;
  final bool isServer;
  final String conversationId;
  final String? state;
  final Model? model;
  final int? totalTokens;
  final int? promptTokens;
  final int? completionTokens;
  final double? responseCost;
  final String? title;

  SSEEvent({
    required this.type,
    this.content,
    this.toolCall,
    this.toolCallId,
    this.toolName,
    this.toolResult,
    this.isServer = false,
    required this.conversationId,
    this.state,
    this.model,
    this.totalTokens,
    this.promptTokens,
    this.completionTokens,
    this.responseCost,
    this.title,
  });

  factory SSEEvent.fromJson(Map<String, dynamic> json) {
    return SSEEvent(
      type: json['type'] ?? '',
      content: json['content'],
      toolCall:
          json['tool_call'] != null
              ? ToolCall.fromJson(json['tool_call'])
              : null,
      toolCallId: json['tool_call_id'],
      toolName: json['tool_name'],
      toolResult: json['tool_result'],
      isServer: json['is_server'] ?? false,
      conversationId: json['conversation_id'] ?? '',
      state: json['state'],
      model: json['model'] != null ? Model.fromJson(json['model']) : null,
      totalTokens: json['total_tokens'],
      promptTokens: json['prompt_tokens'],
      completionTokens: json['completion_tokens'],
      responseCost: (json['response_cost'] as num?)?.toDouble(),
      title: json['title'],
    );
  }
}
