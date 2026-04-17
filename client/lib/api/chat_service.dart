import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/skills_service.dart';

import '../utils/types.dart';
import 'api.dart';
import 'sse_event.dart';
import 'service.dart';

/// A single item in the streaming event log — either text or a tool event.
class StreamItem {
  final String type; // "text", "tool_use", "tool_result"
  final String text;
  final ToolCall? toolCall;
  final String? toolCallId;
  final String? toolResult;
  final bool isServer;

  const StreamItem({
    required this.type,
    this.text = '',
    this.toolCall,
    this.toolCallId,
    this.toolResult,
    this.isServer = false,
  });
}

/// Transient streaming state for a single conversation's active request.
class ChatSessionState {
  final ConversationState state;
  /// Ordered log of all streaming events (text chunks, tool calls, tool results).
  /// Text items are one per LLM turn (reset when tools are called).
  final List<StreamItem> items;
  final List<ToolCall> pendingClientTools;
  final String? error;
  final Model? model;
  final int totalTokens;
  final String? resolvedConversationId;
  final String? title;

  const ChatSessionState({
    this.state = ConversationState.idle,
    this.items = const [],
    this.pendingClientTools = const [],
    this.error,
    this.model,
    this.totalTokens = 0,
    this.resolvedConversationId,
    this.title,
  });

  /// Whether there's any streaming content to show.
  bool get hasContent => items.isNotEmpty;

  /// The current streaming text (last text item, still being appended to).
  String get streamingText {
    for (int i = items.length - 1; i >= 0; i--) {
      if (items[i].type == 'text') return items[i].text;
    }
    return '';
  }

  ChatSessionState copyWith({
    ConversationState? state,
    List<StreamItem>? items,
    List<ToolCall>? pendingClientTools,
    String? error,
    Model? model,
    int? totalTokens,
    String? resolvedConversationId,
    String? title,
  }) {
    return ChatSessionState(
      state: state ?? this.state,
      items: items ?? this.items,
      pendingClientTools: pendingClientTools ?? this.pendingClientTools,
      error: error,
      model: model ?? this.model,
      totalTokens: totalTokens ?? this.totalTokens,
      resolvedConversationId: resolvedConversationId ?? this.resolvedConversationId,
      title: title ?? this.title,
    );
  }
}

/// Lightweight status for a conversation from the global status stream.
class ConversationStatus {
  final String state;    // "processing", "idle", "waiting_for_tool"
  final String activity; // "typing", "tool_use", or ""
  final String toolName; // tool name when activity is "tool_use"

  const ConversationStatus({
    this.state = 'idle',
    this.activity = '',
    this.toolName = '',
  });

  bool get isProcessing => state == 'processing';
  bool get isIdle => state == 'idle';
  bool get isTyping => activity == 'typing';
  bool get isUsingTool => activity == 'tool_use';
}

/// ChatService is a singleton background service that manages all chat
/// processing: SSE connections, tool execution, and state broadcasting.
/// The UI observes this service — it never drives the chat flow.
class ChatService {
  static final ChatService _instance = ChatService._internal();
  factory ChatService() => _instance;
  ChatService._internal();

  final ApiService _api = ApiService();
  final MCPService _mcp = MCPService();
  final SkillsService _skills = SkillsService();

  /// Active SSE stream subscriptions per conversation.
  final Map<String, StreamSubscription> _activeStreams = {};

  /// Observable state per conversation session.
  final Map<String, ValueNotifier<ChatSessionState>> _sessions = {};

  /// Completers for new conversations waiting for their server-assigned ID.
  final Map<String, Completer<String?>> _pendingIdCompleters = {};

  /// Cached server-side tool metadata (name → {loading, icon_url, description}).
  final Map<String, Map<String, String>> _toolMetadataCache = {};

  /// Live conversation statuses from the global status stream.
  final conversationStatuses = ValueNotifier<Map<String, ConversationStatus>>({});
  StreamSubscription? _statusStreamSubscription;

  /// Reference to the conversations notifier (set during app init).
  ConversationsNotifier? _conversationsNotifier;

  void setConversationsNotifier(ConversationsNotifier notifier) {
    _conversationsNotifier = notifier;
  }

  /// Fetch and cache server-side tool metadata.
  /// Call on app start and when conversation list refreshes.
  Future<void> refreshToolMetadata() async {
    try {
      final tools = await _api.getServerTools();
      _toolMetadataCache.clear();
      for (final tool in tools) {
        final name = tool['name'] as String? ?? '';
        if (name.isNotEmpty) {
          _toolMetadataCache[name] = {
            'loading': tool['loading'] as String? ?? '',
            'icon_url': tool['icon_url'] as String? ?? '',
            'description': tool['description'] as String? ?? '',
          };
        }
      }
    } catch (e) {
      debugPrint('[ChatService] Error fetching tool metadata: $e');
    }
  }

  /// Get cached metadata for a tool by name. Returns null if not cached.
  Map<String, String>? getToolMetadata(String toolName) {
    return _toolMetadataCache[toolName];
  }

  /// Get all cached server tool metadata (for model picker UI).
  Map<String, Map<String, String>> get toolMetadata => Map.unmodifiable(_toolMetadataCache);

  /// Get (or create) the observable session state for a conversation.
  ValueNotifier<ChatSessionState> getSession(String conversationId) {
    return _sessions.putIfAbsent(
      conversationId,
      () => ValueNotifier(const ChatSessionState()),
    );
  }

  /// Send a new user message. Connects to the SSE stream automatically.
  /// For new conversations (empty conversationId), returns the server-assigned
  /// conversation ID once the first SSE event arrives. For existing conversations,
  /// returns the same conversationId immediately.
  Future<String?> sendMessage({
    required String conversationId,
    required Message message,
    required ModelSelected modelSelected,
    MiniApp? miniApp,
  }) async {
    final session = getSession(conversationId);
    session.value = const ChatSessionState(state: ConversationState.processing);

    final isNew = conversationId.isEmpty;
    Completer<String?>? idCompleter;
    if (isNew) {
      idCompleter = Completer<String?>();
      _pendingIdCompleters[conversationId] = idCompleter;
    }

    try {
      final skillNames = _skills.getSkillNames();
      final stream = await _api.postChat(
        conversationId: conversationId,
        modelSelected: modelSelected,
        messages: [message],
        miniApp: miniApp,
        clientSideTools: [
          ..._mcp.getToolList(),
          if (skillNames.isNotEmpty) _skills.getToolDefinition(),
        ],
        availableSkills: skillNames,
      );
      _connectSSE(conversationId, stream, modelSelected);
    } catch (e) {
      session.value = ChatSessionState(
        state: ConversationState.idle,
        error: e.toString(),
      );
      idCompleter?.complete(null);
      _pendingIdCompleters.remove(conversationId);
      return null;
    }

    if (isNew && idCompleter != null) {
      return idCompleter.future;
    }
    return conversationId;
  }

  /// Submit client-side tool results back to the server.
  Future<void> submitToolResults({
    required String conversationId,
    required List<Message> toolResults,
    required ModelSelected modelSelected,
  }) async {
    final session = getSession(conversationId);
    session.value = session.value.copyWith(
      state: ConversationState.processing,
      pendingClientTools: [],
    );

    try {
      final skillNames = _skills.getSkillNames();
      final stream = await _api.postChat(
        conversationId: conversationId,
        modelSelected: modelSelected,
        toolResults: toolResults,
        clientSideTools: [
          ..._mcp.getToolList(),
          if (skillNames.isNotEmpty) _skills.getToolDefinition(),
        ],
        availableSkills: skillNames,
      );
      _connectSSE(conversationId, stream, modelSelected);
    } catch (e) {
      session.value = session.value.copyWith(
        state: ConversationState.idle,
        error: e.toString(),
      );
    }
  }

  /// Cancel an active request.
  Future<void> cancel(String conversationId) async {
    try {
      await _api.cancelChat(conversationId);
    } catch (_) {}
    _disconnectSSE(conversationId);
    final session = getSession(conversationId);
    session.value = session.value.copyWith(state: ConversationState.idle);
  }

  /// Reconnect to an active conversation's SSE stream.
  Future<void> reconnect(String conversationId) async {
    if (_activeStreams.containsKey(conversationId)) return;

    try {
      final stream = await _api.connectStream(conversationId);
      _connectSSE(conversationId, stream, null);
    } catch (_) {
      // No active stream on server — conversation is idle
    }
  }

  bool _connected = false;

  /// Call from build() — safe to call repeatedly, only connects once.
  void ensureConnected() {
    if (_connected) return;
    _connected = true;
    connectStatusStream();
  }

  /// Connect to the global status stream. Call once on app start (after auth).
  Future<void> connectStatusStream() async {
    // Refresh tool metadata cache
    await refreshToolMetadata();

    // Cancel existing status stream if any
    _statusStreamSubscription?.cancel();

    try {
      final stream = await _api.connectStatusStream();
      _statusStreamSubscription = stream.listen(
        (data) {
          final conversationId = data['conversation_id'] as String? ?? '';
          if (conversationId.isEmpty) return;

          // Handle server-generated title/icon pushed via status stream
          final title = data['title'] as String?;
          final icon = data['icon'] as String?;
          if (title != null && title.isNotEmpty) {
            _conversationsNotifier?.updateConversationMetaData(
              conversationId: conversationId,
              title: title,
              icon: icon,
            );
          }

          final status = ConversationStatus(
            state: data['state'] as String? ?? 'idle',
            activity: data['activity'] as String? ?? '',
            toolName: data['tool_name'] as String? ?? '',
          );

          final updated = Map<String, ConversationStatus>.from(conversationStatuses.value);
          if (status.isIdle) {
            updated.remove(conversationId);
          } else {
            updated[conversationId] = status;
            // If we don't know this conversation locally, refresh the list
            if (_conversationsNotifier != null) {
              final known = _conversationsNotifier!.state.conversations.any((c) => c.id == conversationId);
              if (!known) _conversationsNotifier!.refresh();
            }
          }
          conversationStatuses.value = updated;
        },
        onError: (e) {
          debugPrint('[ChatService] Status stream error: $e');
          // Reconnect after a delay
          Future.delayed(const Duration(seconds: 5), connectStatusStream);
        },
        onDone: () {
          // Stream closed — reconnect
          Future.delayed(const Duration(seconds: 2), connectStatusStream);
        },
      );
    } catch (e) {
      debugPrint('[ChatService] Failed to connect status stream: $e');
    }
  }

  // --- Internal ---

  void _connectSSE(
    String initialConversationId,
    Stream<SSEEvent> stream,
    ModelSelected? modelSelected,
  ) {
    _disconnectSSE(initialConversationId);

    // Use a mutable reference so re-keying in _handleSSEEvent is reflected
    // in subsequent events from the same listener.
    String currentId = initialConversationId;

    final subscription = stream.listen(
      (event) {
        currentId = _handleSSEEvent(currentId, event, modelSelected);
      },
      onError: (error) {
        debugPrint('[ChatService] SSE error for $currentId: $error');
        final session = getSession(currentId);
        session.value = session.value.copyWith(
          state: ConversationState.idle,
          error: error.toString(),
        );
        _activeStreams.remove(currentId);
      },
      onDone: () {
        _activeStreams.remove(currentId);
      },
    );

    _activeStreams[initialConversationId] = subscription;
  }

  void _disconnectSSE(String conversationId) {
    _activeStreams[conversationId]?.cancel();
    _activeStreams.remove(conversationId);
  }

  /// Handles a single SSE event. Returns the (potentially updated) conversation ID
  /// to be used for subsequent events from the same stream.
  String _handleSSEEvent(
    String conversationId,
    SSEEvent event,
    ModelSelected? modelSelected,
  ) {
    final session = getSession(conversationId);

    // Detect new conversation ID from server (for new conversations)
    if (event.conversationId.isNotEmpty &&
        event.conversationId != conversationId &&
        session.value.resolvedConversationId == null) {
      final realId = event.conversationId;
      session.value = session.value.copyWith(resolvedConversationId: realId);

      // Create the conversation in local state
      if (modelSelected != null) {
        _conversationsNotifier?.createConversation(
          id: realId,
          title: event.title ?? 'New Chat',
          modelSelected: modelSelected,
        );
      }

      // Re-key the session and stream under the real ID
      _sessions[realId] = session;
      _sessions.remove(conversationId);
      if (_activeStreams.containsKey(conversationId)) {
        _activeStreams[realId] = _activeStreams.remove(conversationId)!;
      }

      // Complete the pending ID completer so the UI can navigate
      final completer = _pendingIdCompleters.remove(conversationId);
      if (completer != null && !completer.isCompleted) {
        completer.complete(realId);
      }

      conversationId = realId;
    }

    // Update title if received
    if (event.title != null && event.title!.isNotEmpty) {
      session.value = session.value.copyWith(title: event.title);
    }

    switch (event.type) {
      case 'text':
        // Append to the current text item, or create one if the last item isn't text
        final items = List<StreamItem>.from(session.value.items);
        if (items.isNotEmpty && items.last.type == 'text') {
          items[items.length - 1] = StreamItem(
            type: 'text',
            text: items.last.text + (event.content ?? ''),
          );
        } else {
          items.add(StreamItem(type: 'text', text: event.content ?? ''));
        }
        session.value = session.value.copyWith(
          items: items,
          model: event.model ?? session.value.model,
          totalTokens: event.totalTokens ?? session.value.totalTokens,
        );
        break;

      case 'tool_use':
        final items = List<StreamItem>.from(session.value.items);
        items.add(StreamItem(
          type: 'tool_use',
          toolCall: event.toolCall,
          isServer: event.isServer,
        ));
        if (event.toolCall != null) {
          // Check if this tool is in "ask" mode — if so, don't auto-queue it
          final toolsMap = modelSelected?.text?.tools;
          final isAsk = toolsMap != null && toolsMap[event.toolCall!.function.name] == 'ask';
          if (isAsk) {
            // Just add to items — the approval banner derives from DB state
            session.value = session.value.copyWith(items: items);
          } else if (event.isServer) {
            session.value = session.value.copyWith(items: items);
          } else {
            session.value = session.value.copyWith(
              items: items,
              pendingClientTools: [...session.value.pendingClientTools, event.toolCall!],
            );
          }
        }
        break;

      case 'tool_result':
        final items = List<StreamItem>.from(session.value.items);
        items.add(StreamItem(
          type: 'tool_result',
          toolCallId: event.toolCallId,
          toolResult: event.toolResult,
          isServer: event.isServer,
        ));
        session.value = session.value.copyWith(items: items);
        break;

      case 'state_change':
        final newState = conversationStateFromString(event.state);
        session.value = session.value.copyWith(state: newState);

        if (newState == ConversationState.waitingForTool && modelSelected != null) {
          if (session.value.pendingClientTools.isNotEmpty) {
            // Auto-execute non-ask client tools
            _executeClientTools(conversationId, modelSelected);
          }
          // If pendingAskTools is non-empty, the UI will show an approval banner
        }
        break;

      case 'done':
        _finalizeSession(conversationId, event.title);
        break;

      case 'error':
        session.value = session.value.copyWith(
          state: ConversationState.idle,
          error: event.content,
        );
        break;
    }

    return conversationId;
  }

  /// Execute client-side (MCP) tools and submit results back to server.
  Future<void> _executeClientTools(
    String conversationId,
    ModelSelected modelSelected,
  ) async {
    final session = getSession(conversationId);
    final toolCalls = List<ToolCall>.from(session.value.pendingClientTools);
    final results = <Message>[];

    for (final toolCall in toolCalls) {
      try {
        // Handle retrieve_skill locally (not an MCP tool)
        if (toolCall.function.name == 'retrieve_skill') {
          final args = jsonDecode(
            toolCall.function.arguments.isEmpty
                ? '{}'
                : toolCall.function.arguments,
          );
          final content = await _skills.executeRetrieveSkill(
            args['skill_name'] as String? ?? '',
            args['file_name'] as String?,
          );
          results.add(Message.toolResult(
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            result: content,
          ));
          continue;
        }

        final serverName = _mcp.getToolServerName(toolCall.function.name);
        if (serverName == null) {
          results.add(Message.toolResult(
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            result: 'Error: MCP server not found for tool ${toolCall.function.name}',
          ));
          continue;
        }

        final args = jsonDecode(
          toolCall.function.arguments.isEmpty
              ? '{}'
              : toolCall.function.arguments,
        );
        final response = await _mcp.serverManager.sendRequest(
          serverName,
          'tools/call',
          {'name': toolCall.function.name, 'arguments': args},
          conversationId,
        );

        results.add(Message.toolResult(
          toolCallId: toolCall.id,
          name: toolCall.function.name,
          result: jsonEncode(response),
        ));
      } catch (e) {
        results.add(Message.toolResult(
          toolCallId: toolCall.id,
          name: toolCall.function.name,
          result: 'Error: $e',
        ));
      }
    }

    if (results.isNotEmpty) {
      // Add tool result messages to local state
      for (final result in results) {
        _conversationsNotifier?.addMessage(
          conversationId: conversationId,
          message: result,
        );
      }

      await submitToolResults(
        conversationId: conversationId,
        toolResults: results,
        modelSelected: modelSelected,
      );
    }
  }

  /// Handle user approval/denial of "ask" tools.
  /// [decisions] maps tool call ID → approved (true/false).
  /// Server-side tools are sent to /chat/approve (server executes + relaunches loop).
  /// Client-side tools are executed locally and submitted via HandleChat + ToolResults.
  Future<void> approveTools({
    required String conversationId,
    required ModelSelected modelSelected,
    required Map<String, bool> decisions,
    required List<ToolCall> askTools,
  }) async {
    if (askTools.isEmpty) return;

    final session = getSession(conversationId);
    session.value = session.value.copyWith(
      state: ConversationState.processing,
    );

    // Split into server-side and client-side tools
    final serverApprovals = <Map<String, dynamic>>[];
    final clientAskTools = <ToolCall>[];

    for (final toolCall in askTools) {
      final approved = decisions[toolCall.id] ?? false;
      final isClientTool = _mcp.getToolServerName(toolCall.function.name) != null
          || toolCall.function.name == 'retrieve_skill';
      if (isClientTool) {
        clientAskTools.add(toolCall);
      } else {
        serverApprovals.add({
          'tool_call_id': toolCall.id,
          'tool_name': toolCall.function.name,
          'arguments': toolCall.function.arguments,
          'approved': approved,
        });
      }
    }

    // Handle client-side ask tools locally
    final clientResults = <Message>[];
    for (final toolCall in clientAskTools) {
      final approved = decisions[toolCall.id] ?? false;
      if (approved) {
        try {
          if (toolCall.function.name == 'retrieve_skill') {
            final args = jsonDecode(
              toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments,
            );
            final content = await _skills.executeRetrieveSkill(
              args['skill_name'] as String? ?? '',
              args['file_name'] as String?,
            );
            clientResults.add(Message.toolResult(
              toolCallId: toolCall.id, name: toolCall.function.name, result: content,
            ));
          } else {
            final serverName = _mcp.getToolServerName(toolCall.function.name)!;
            final args = jsonDecode(
              toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments,
            );
            final response = await _mcp.serverManager.sendRequest(
              serverName, 'tools/call',
              {'name': toolCall.function.name, 'arguments': args},
              conversationId,
            );
            clientResults.add(Message.toolResult(
              toolCallId: toolCall.id, name: toolCall.function.name, result: jsonEncode(response),
            ));
          }
        } catch (e) {
          clientResults.add(Message.toolResult(
            toolCallId: toolCall.id, name: toolCall.function.name, result: 'Error: $e',
          ));
        }
      } else {
        clientResults.add(Message.toolResult(
          toolCallId: toolCall.id, name: toolCall.function.name,
          result: 'Tool call rejected by user.',
        ));
      }
    }

    // If there are server-side approvals, use /chat/approve (it relaunches the loop)
    if (serverApprovals.isNotEmpty) {
      // If we also have client results, submit those first via HandleChat
      if (clientResults.isNotEmpty) {
        for (final result in clientResults) {
          _conversationsNotifier?.addMessage(conversationId: conversationId, message: result);
        }
        // Push client results to DB without relaunching loop
        await submitToolResults(
          conversationId: conversationId,
          toolResults: clientResults,
          modelSelected: modelSelected,
        );
      }
      // Then approve server tools (this relaunches the loop + SSE)
      try {
        final skillNames = _skills.getSkillNames();
        final stream = await _api.approveTools(
          conversationId: conversationId,
          approvals: serverApprovals,
          modelSelected: modelSelected,
          clientSideTools: [
            ..._mcp.getToolList(),
            if (skillNames.isNotEmpty) _skills.getToolDefinition(),
          ],
          availableSkills: skillNames,
        );
        _connectSSE(conversationId, stream, modelSelected);
      } catch (e) {
        session.value = session.value.copyWith(
          state: ConversationState.idle, error: e.toString(),
        );
      }
    } else if (clientResults.isNotEmpty) {
      // Only client-side tools — submit via existing HandleChat + ToolResults
      for (final result in clientResults) {
        _conversationsNotifier?.addMessage(conversationId: conversationId, message: result);
      }
      await submitToolResults(
        conversationId: conversationId,
        toolResults: clientResults,
        modelSelected: modelSelected,
      );
    }
  }

  /// After a stream ends, reload conversation from server and reset session.
  Future<void> _finalizeSession(String conversationId, String? title) async {
    final session = getSession(conversationId);
    final resolvedId = session.value.resolvedConversationId ?? conversationId;

    // Reset session state
    session.value = ChatSessionState(
      state: ConversationState.idle,
      resolvedConversationId: session.value.resolvedConversationId,
    );

    // Reload conversation from server to get the full DB-saved messages
    if (resolvedId.isNotEmpty) {
      _conversationsNotifier?.loadConversation(resolvedId);
    }

    // Title generation is now handled server-side and pushed via the status stream
  }
}
