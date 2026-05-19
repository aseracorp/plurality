import 'dart:async';
import 'dart:convert';
import 'dart:io' show Platform;
import 'package:flutter/foundation.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/skills_service.dart';
import 'package:plurality/api/filesystem_service.dart';
import 'package:plurality/api/shell_service.dart';
import 'package:plurality/api/client_identity.dart';

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
  final FilesystemService _filesystem = FilesystemService();
  final ShellService _shell = ShellService();

  /// True on the three desktop platforms where `Process.start` works. The
  /// device-side shell tool is only advertised there — on mobile the tool is
  /// invisible to the LLM.
  static bool get _isDesktop =>
      Platform.isWindows || Platform.isMacOS || Platform.isLinux;

  /// Resolve the attached folder path from the per-conversation model
  /// selection. Empty/null means no folder is attached, which is what gates
  /// (a) whether the device-side filesystem tool schemas are sent to the LLM
  /// and (b) the sandbox root for client-side filesystem tool execution.
  String? _folderFromModel(ModelSelected? modelSelected) {
    final p = modelSelected?.clientFolderPath;
    if (p == null || p.isEmpty) return null;
    return p;
  }

  /// Active SSE stream subscriptions per conversation.
  final Map<String, StreamSubscription> _activeStreams = {};

  /// Conversation ids for which a reconnect() is mid-flight (HTTP in flight,
  /// not yet registered in [_activeStreams]). Used to serialize concurrent
  /// reconnect attempts — without this, two callers both observe "no entry
  /// in _activeStreams" and both attach SSE clients to the same server-side
  /// ActiveRequest, which broadcasts every event to both → duplicate text.
  final Set<String> _connecting = {};

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
      final attachedFolder = _folderFromModel(modelSelected);
      final stream = await _api.postChat(
        conversationId: conversationId,
        modelSelected: modelSelected,
        messages: [message],
        miniApp: miniApp,
        clientSideTools: [
          ..._mcp.getToolList(),
          if (skillNames.isNotEmpty) _skills.getToolDefinition(),
          if (attachedFolder != null) ..._filesystem.getToolDefinitions(),
          if (_isDesktop)
            _shell.getToolDefinition(attachedFolder: attachedFolder),
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
      final attachedFolder = _folderFromModel(modelSelected);
      final stream = await _api.postChat(
        conversationId: conversationId,
        modelSelected: modelSelected,
        toolResults: toolResults,
        clientSideTools: [
          ..._mcp.getToolList(),
          if (skillNames.isNotEmpty) _skills.getToolDefinition(),
          if (attachedFolder != null) ..._filesystem.getToolDefinitions(),
          if (_isDesktop)
            _shell.getToolDefinition(attachedFolder: attachedFolder),
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
    // Cancelling a Dart StreamSubscription doesn't fire onError/onDone, so
    // the SSE error path (which normally clears global status) is skipped.
    // Wipe the entry here so the chat UI's isProcessing check goes false.
    _clearStatus(conversationId);
  }

  /// Reset a conversation's session to a clean idle state when there is no
  /// live SSE attached. Keeps the resolved id for new-conversation flows.
  /// Without this, items from a previous interrupted stream stay in the
  /// ValueNotifier and render on top of the freshly-loaded DB messages.
  void resetSessionIfIdle(String conversationId) {
    if (conversationId.isEmpty) return;
    if (_activeStreams.containsKey(conversationId)) return;
    final existing = _sessions[conversationId];
    if (existing == null) return;
    existing.value = ChatSessionState(
      state: ConversationState.idle,
      resolvedConversationId: existing.value.resolvedConversationId,
    );
  }

  /// Tracks conversations for which a resume attempt is already in flight,
  /// so concurrent triggers (loadConversation + reconnect + status events)
  /// don't double-dispatch the same trailing tool calls.
  final Set<String> _resumingClientTools = {};

  /// True for tool names this client knows how to dispatch — the device-side
  /// filesystem / shell tools, retrieve_skill, and any registered MCP tool.
  /// Used by the trailing-tool resume path to decide whether a pending tool
  /// call is ours to handle vs. a server-side tool waiting on something else.
  bool _isClientSideToolName(String name) {
    if (name == FilesystemService.readToolName ||
        name == FilesystemService.writeToolName ||
        name == ShellService.toolName ||
        name == 'retrieve_skill') {
      return true;
    }
    return _mcp.getToolServerName(name) != null;
  }

  /// If the just-loaded conversation has an assistant message whose
  /// client-side tool calls were never answered (e.g. the previous client
  /// crashed or was closed mid-execution), queue them and dispatch them
  /// from this client. Guards against double-dispatch with:
  ///   - a live SSE stream attached (let the stream drive execution)
  ///   - the session already processing (a send / submit is in flight)
  ///   - pendingClientTools already populated (executing or about to)
  ///   - an in-flight resume for the same conversation
  ///   - the conversation's clientLock pointing at a different client (the
  ///     lock holder is the one expected to run it; we let them).
  /// Safe to call on every conversation open / refresh — it short-circuits
  /// when there's nothing to do.
  Future<void> resumeTrailingClientToolsIfNeeded(String conversationId) async {
    if (conversationId.isEmpty) return;
    if (_resumingClientTools.contains(conversationId)) return;
    if (_activeStreams.containsKey(conversationId)) return;
    final session = getSession(conversationId);
    if (session.value.state == ConversationState.processing) return;
    if (session.value.pendingClientTools.isNotEmpty) return;

    Conversation? conv;
    final convs = _conversationsNotifier?.state.conversations;
    if (convs != null) {
      for (final c in convs) {
        if (c.id == conversationId) {
          conv = c;
          break;
        }
      }
    }
    if (conv == null || conv.messages.isEmpty) return;

    // Lock arbitration: if someone else owns the lock, let them resume.
    final lock = conv.modelSelected.clientLock;
    final myId = ClientIdentity().id;
    if (lock != null && myId.isNotEmpty && lock.id != myId) return;

    // Collect tool_call ids that already have matching tool result messages.
    final resolved = <String>{};
    for (final m in conv.messages) {
      if (m.role == 'tool' && m.toolCallId != null) {
        resolved.add(m.toolCallId!);
      }
    }

    // Find the most recent assistant message with tool calls — that's the
    // turn the server is waiting on. Earlier orphaned tool calls (if any)
    // belong to past turns and aren't recoverable here.
    Message? trailingAssistant;
    for (int i = conv.messages.length - 1; i >= 0; i--) {
      final m = conv.messages[i];
      if (m.role == 'assistant' &&
          m.toolCalls != null &&
          m.toolCalls!.isNotEmpty) {
        trailingAssistant = m;
        break;
      }
    }
    if (trailingAssistant == null) return;

    final pending = <ToolCall>[];
    for (final tc in trailingAssistant.toolCalls!) {
      if (resolved.contains(tc.id)) continue;
      if (_isClientSideToolName(tc.function.name)) {
        pending.add(tc);
      }
    }
    if (pending.isEmpty) return;

    _resumingClientTools.add(conversationId);
    try {
      // Mirror the state the SSE state_change → waitingForTool would have
      // landed the session in, then hand off to the standard executor —
      // which will stamp our lock onto the outgoing modelSelected and POST
      // tool results, same as the live-stream path.
      session.value = session.value.copyWith(
        state: ConversationState.waitingForTool,
        pendingClientTools: pending,
      );
      await _executeClientTools(conversationId, conv.modelSelected);
    } finally {
      _resumingClientTools.remove(conversationId);
    }
  }

  /// Reconnect to an active conversation's SSE stream. Idempotent.
  /// Safe to call on conversation open, on SSE error, and on app resume.
  /// If the server has no active stream, resolves the session to idle and
  /// clears any stale entry in [conversationStatuses].
  Future<void> reconnect(String conversationId) async {
    if (conversationId.isEmpty) return;
    if (_activeStreams.containsKey(conversationId)) return;
    // Another caller is already attaching — wait for it instead of racing.
    if (_connecting.contains(conversationId)) return;
    // sendMessage / submitToolResults set state=processing synchronously
    // before awaiting their POST. While that POST is in flight we don't
    // have an entry in _activeStreams yet, but reconnecting would race the
    // POST's stream with our /chat/stream/<id> stream — and the server
    // would fan every event to both sockets. Bail.
    final existing = _sessions[conversationId];
    if (existing != null &&
        existing.value.state == ConversationState.processing) {
      return;
    }

    _connecting.add(conversationId);
    try {
      final stream = await _api.connectStream(conversationId);
      // Live stream — flip session to processing so the UI shows activity
      // immediately rather than waiting for the first event.
      final session = getSession(conversationId);
      session.value = session.value.copyWith(
        state: ConversationState.processing,
      );
      _connectSSE(conversationId, stream, null);
    } catch (_) {
      // No active stream on server — force local state to idle so a stale
      // "processing" UI doesn't get stuck.
      final session = getSession(conversationId);
      session.value = session.value.copyWith(state: ConversationState.idle);
      _clearStatus(conversationId);
    } finally {
      _connecting.remove(conversationId);
    }
  }

  /// Remove a conversation's entry from [conversationStatuses] when the
  /// global view is known to be stale (e.g. after an SSE abort).
  void _clearStatus(String conversationId) {
    final current = conversationStatuses.value;
    if (!current.containsKey(conversationId)) return;
    final updated = Map<String, ConversationStatus>.from(current);
    updated.remove(conversationId);
    conversationStatuses.value = updated;
  }

  /// Called from the app lifecycle observer when the app resumes. The OS
  /// may have severed the SSE sockets while backgrounded; flush the global
  /// status stream so it (and the events it emits) can drive recovery. The
  /// currently-viewed chat is reattached separately by the ChatInterface's
  /// own lifecycle observer.
  Future<void> handleAppResumed() async {
    _statusStreamSubscription?.cancel();
    _statusStreamSubscription = null;
    connectStatusStream();
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

          // Any status event for an unknown conversation means the sidebar is
          // stale — sub-agents and AI-created conversations don't go through
          // the regular client-initiated SSE flow, so the conversation row
          // never reaches local state otherwise. Refresh regardless of state
          // (idle events fire for newly-created inert conversations and for
          // sub-agents that finished between our last refresh and now).
          final known = _conversationsNotifier?.state.conversations
                  .any((c) => c.id == conversationId) ??
              false;
          if (!known) {
            _conversationsNotifier?.refresh();
          }

          // Handle server-generated title/icon pushed via status stream
          final title = data['title'] as String?;
          final icon = data['icon'] as String?;
          final modelSelectedJson = data['model_selected'];
          ModelSelected? incomingModelSelected;
          if (modelSelectedJson is Map) {
            try {
              incomingModelSelected = ModelSelected.fromJson(
                Map<String, dynamic>.from(modelSelectedJson),
              );
            } catch (_) {
              // Malformed payload — skip the merge rather than poisoning state.
            }
          }
          if ((title != null && title.isNotEmpty) ||
              (icon != null && icon.isNotEmpty) ||
              incomingModelSelected != null) {
            _conversationsNotifier?.updateConversationMetaData(
              conversationId: conversationId,
              title: title,
              icon: icon,
              modelSelected: incomingModelSelected,
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
          }
          conversationStatuses.value = updated;

          // If the server says this conversation is processing but our SSE
          // socket for it is gone (e.g. it died while we were backgrounded),
          // reattach. Limited to conversations we already have a local
          // session for — we don't want to spin up SSE streams for chats
          // the user hasn't opened. reconnect() is a no-op when the stream
          // is already healthy.
          //
          // Skip when the local session is already processing: that means
          // sendMessage/submitToolResults set state=processing synchronously
          // and is mid-await on the POST. Reconnecting here would race the
          // POST's stream and double-up every event between the two sockets.
          if (!status.isIdle &&
              _sessions.containsKey(conversationId) &&
              !_activeStreams.containsKey(conversationId) &&
              _sessions[conversationId]!.value.state !=
                  ConversationState.processing) {
            reconnect(conversationId);
          }
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
      debugPrint('[ChatService] Failed to connect status stream: $e — retrying in 5s');
      Future.delayed(const Duration(seconds: 5), connectStatusStream);
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
        // After a transient socket abort (e.g. phone locked, network blip)
        // the conversation may still be running server-side. Try to reattach
        // shortly; reconnect() falls through to idle if the server says
        // there's no active stream.
        _clearStatus(currentId);
        final convId = currentId;
        Future.delayed(const Duration(seconds: 2), () {
          if (!_activeStreams.containsKey(convId)) {
            reconnect(convId);
          }
        });
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

      // Create the conversation in local state. The model_selected payload
      // (which carries the per-conversation tool toggles and the attached
      // folder path) is round-tripped server-side, so the local row will
      // hydrate naturally on the next loadConversation.
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

    // Sync per-conversation settings (tools / folder / eco / client lock)
    // from any server event that carries the snapshot. The server stamps
    // model_selected on tool_use and done events; here we mirror that into
    // the local conversation in Riverpod so UI watchers (lock badge,
    // banner, eco toggle, folder chip) reflect the freshest state — and
    // so a passively-viewing client sees the lock holder *before* it
    // would otherwise race to execute the same client-side tool.
    if (event.modelSelected != null) {
      _conversationsNotifier?.updateConversationMetaData(
        conversationId: conversationId,
        modelSelected: event.modelSelected,
      );
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

        if (newState == ConversationState.waitingForTool &&
            session.value.pendingClientTools.isNotEmpty) {
          // The modelSelected parameter is the one captured when this SSE
          // stream was opened. It's null whenever the stream was attached
          // via reconnect() — most importantly when another client sent
          // the message and we hopped onto its ActiveRequest via the
          // status stream. In that case fall back to the live conversation
          // in Riverpod (kept fresh by the model_selected stamped on every
          // tool_use / done event). Without this fallback the lock holder
          // receives the tool call but never dispatches it.
          ModelSelected? effective = modelSelected;
          if (effective == null) {
            final convs = _conversationsNotifier?.state.conversations;
            if (convs != null) {
              for (final c in convs) {
                if (c.id == conversationId) {
                  effective = c.modelSelected;
                  break;
                }
              }
            }
          }
          if (effective != null) {
            _executeClientTools(conversationId, effective);
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

  /// Builds the same client-side tool definition list the server received in
  /// this request, so we can validate tool-call args against the very
  /// schemas the LLM was shown.
  List<Map<String, dynamic>> _currentClientToolDefs(ModelSelected modelSelected) {
    final skillNames = _skills.getSkillNames();
    final attachedFolder = _folderFromModel(modelSelected);
    return [
      ..._mcp.getToolList(),
      if (skillNames.isNotEmpty) _skills.getToolDefinition(),
      if (attachedFolder != null) ..._filesystem.getToolDefinitions(),
      if (_isDesktop)
        _shell.getToolDefinition(attachedFolder: attachedFolder),
    ];
  }

  /// Strict-validate args for a client-side tool call against its declared
  /// schema. Returns null when valid, or an LLM-facing error string. Mirrors
  /// the server's ai_tools.ValidateToolCallArgs.
  String? _validateClientToolArgs(
    ToolCall toolCall,
    List<Map<String, dynamic>> defs,
  ) {
    Map<String, dynamic>? def;
    for (final d in defs) {
      if (d['name'] == toolCall.function.name) {
        def = d;
        break;
      }
    }
    if (def == null) return null;
    final params = (def['parameters'] ?? def['input_schema']) as Map?;
    if (params == null) return null;

    final propsRaw = params['properties'];
    final allowed = propsRaw is Map ? propsRaw.keys.map((k) => k.toString()).toSet() : <String>{};
    final requiredRaw = params['required'];
    final required = requiredRaw is List
        ? requiredRaw.map((e) => e.toString()).toList()
        : <String>[];

    final argsStr = toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments;
    dynamic decoded;
    try {
      decoded = jsonDecode(argsStr);
    } catch (e) {
      return 'Error: invalid arguments for ${toolCall.function.name}: arguments must be a JSON object ($e)';
    }
    if (decoded is! Map) {
      return 'Error: invalid arguments for ${toolCall.function.name}: arguments must be a JSON object';
    }
    final provided = decoded.keys.map((k) => k.toString()).toSet();

    final unknown = provided.difference(allowed).toList()..sort();
    final missing = required.where((r) => !provided.contains(r)).toList()..sort();
    if (unknown.isEmpty && missing.isEmpty) return null;

    final allowedSorted = allowed.toList()..sort();
    final parts = <String>[];
    if (unknown.isNotEmpty) parts.add('unknown parameter(s) [${unknown.join(', ')}]');
    if (missing.isNotEmpty) parts.add('missing required parameter(s) [${missing.join(', ')}]');
    return 'Error: invalid arguments for ${toolCall.function.name}: ${parts.join('; ')}; allowed parameters: [${allowedSorted.join(', ')}]';
  }

  /// Execute client-side (MCP) tools and submit results back to server.
  Future<void> _executeClientTools(
    String conversationId,
    ModelSelected modelSelected,
  ) async {
    final session = getSession(conversationId);

    // Client-lock gate. The conversation can only have one machine
    // dispatching its client-side tools at a time. Other connected
    // clients still receive the tool_use events (so the stream stays
    // visible in the UI) but must skip execution — the locked client
    // will submit the results and the server doesn't care which
    // client posts them.
    //
    // Important: we re-read the lock from Riverpod rather than trusting
    // the `modelSelected` argument the caller captured when starting the
    // request. The server stamps the current lock onto tool_use events,
    // and our SSE handler mirrors that into Riverpod *before* the
    // state_change → waitingForTool that triggers this method. So the
    // fresh read is what closes the race when another client has just
    // claimed the conversation.
    Conversation? liveConv;
    final convs = _conversationsNotifier?.state.conversations;
    if (convs != null) {
      for (final c in convs) {
        if (c.id == conversationId) {
          liveConv = c;
          break;
        }
      }
    }
    final liveModelSelected = liveConv?.modelSelected ?? modelSelected;

    final myId = ClientIdentity().id;
    final lock = liveModelSelected.clientLock;
    if (lock != null && myId.isNotEmpty && lock.id != myId) {
      // Drop the queued calls so we don't try to re-dispatch them on the
      // next state_change. The locked client's submission will advance
      // the conversation.
      session.value =
          session.value.copyWith(pendingClientTools: const []);
      return;
    }
    // Adopt the live model so any folder / eco / tool changes pushed by
    // the server are honoured during this execution too. Lock acquisition
    // itself is deferred until the tool-results submission below — that's
    // the moment we actually claim ownership over the wire, and the
    // server arbitrates if two clients race.
    modelSelected = liveModelSelected;

    final toolCalls = List<ToolCall>.from(session.value.pendingClientTools);
    final results = <Message>[];
    final toolDefs = _currentClientToolDefs(modelSelected);

    for (final toolCall in toolCalls) {
      final validationError = _validateClientToolArgs(toolCall, toolDefs);
      if (validationError != null) {
        results.add(Message.toolResult(
          toolCallId: toolCall.id,
          name: toolCall.function.name,
          result: validationError,
        ));
        continue;
      }
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

        // Device-side filesystem tools (sandboxed to the conversation's
        // attached folder).
        if (toolCall.function.name == FilesystemService.readToolName ||
            toolCall.function.name == FilesystemService.writeToolName) {
          final args = jsonDecode(
            toolCall.function.arguments.isEmpty
                ? '{}'
                : toolCall.function.arguments,
          ) as Map<String, dynamic>;
          final root = _folderFromModel(modelSelected);
          final result = toolCall.function.name == FilesystemService.readToolName
              ? await _filesystem.executeFsRead(root, args)
              : await _filesystem.executeFsWrite(root, args);
          results.add(Message.toolResult(
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            result: result,
          ));
          continue;
        }

        // Device-side shell. cwd defaults to the attached folder if set,
        // else the user's home directory; absolute pwd values are honored.
        if (toolCall.function.name == ShellService.toolName) {
          final args = jsonDecode(
            toolCall.function.arguments.isEmpty
                ? '{}'
                : toolCall.function.arguments,
          ) as Map<String, dynamic>;
          final root = _folderFromModel(modelSelected);
          final result = await _shell.executeShellExec(root, args);
          results.add(Message.toolResult(
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            result: result,
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
          {'name': MCPService.bareToolName(toolCall.function.name), 'arguments': args},
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
      // Stamp this client as the lock holder on the outgoing modelSelected.
      // The server's PushMessage will persist this, and the next SSE event
      // back (tool_use / done with model_selected) will mirror it into
      // Riverpod on every viewer — including us — so the badge appears
      // without a separate local write. If two clients race to reply, the
      // POST that lands last wins and the loser's local lock state is
      // overwritten by the server's reply, which is the correct UX.
      // We also do an optimistic local update so the badge surfaces
      // immediately on this client without waiting for the round-trip.
      final claim = ClientIdentity().asLock();
      if (claim != null &&
          (modelSelected.clientLock == null ||
              modelSelected.clientLock!.id != claim.id)) {
        modelSelected = modelSelected.copyWith(clientLock: claim);
        _conversationsNotifier?.updateConversationMetaData(
          conversationId: conversationId,
          modelSelected: modelSelected,
        );
      }

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
      final name = toolCall.function.name;
      final isClientTool = _mcp.getToolServerName(name) != null
          || name == 'retrieve_skill'
          || name == FilesystemService.readToolName
          || name == FilesystemService.writeToolName
          || name == ShellService.toolName;
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
    final clientToolDefs = _currentClientToolDefs(modelSelected);
    for (final toolCall in clientAskTools) {
      final approved = decisions[toolCall.id] ?? false;
      if (approved) {
        final validationError = _validateClientToolArgs(toolCall, clientToolDefs);
        if (validationError != null) {
          clientResults.add(Message.toolResult(
            toolCallId: toolCall.id, name: toolCall.function.name, result: validationError,
          ));
          continue;
        }
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
          } else if (toolCall.function.name == FilesystemService.readToolName ||
              toolCall.function.name == FilesystemService.writeToolName) {
            final args = jsonDecode(
              toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments,
            ) as Map<String, dynamic>;
            final root = _folderFromModel(modelSelected);
            final result = toolCall.function.name == FilesystemService.readToolName
                ? await _filesystem.executeFsRead(root, args)
                : await _filesystem.executeFsWrite(root, args);
            clientResults.add(Message.toolResult(
              toolCallId: toolCall.id, name: toolCall.function.name, result: result,
            ));
          } else if (toolCall.function.name == ShellService.toolName) {
            final args = jsonDecode(
              toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments,
            ) as Map<String, dynamic>;
            final root = _folderFromModel(modelSelected);
            final result = await _shell.executeShellExec(root, args);
            clientResults.add(Message.toolResult(
              toolCallId: toolCall.id, name: toolCall.function.name, result: result,
            ));
          } else {
            final serverName = _mcp.getToolServerName(toolCall.function.name)!;
            final args = jsonDecode(
              toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments,
            );
            final response = await _mcp.serverManager.sendRequest(
              serverName, 'tools/call',
              {'name': MCPService.bareToolName(toolCall.function.name), 'arguments': args},
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
        final attachedFolder = _folderFromModel(modelSelected);
        final stream = await _api.approveTools(
          conversationId: conversationId,
          approvals: serverApprovals,
          modelSelected: modelSelected,
          clientSideTools: [
            ..._mcp.getToolList(),
            if (skillNames.isNotEmpty) _skills.getToolDefinition(),
            if (attachedFolder != null) ..._filesystem.getToolDefinitions(),
            if (_isDesktop)
              _shell.getToolDefinition(attachedFolder: attachedFolder),
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
