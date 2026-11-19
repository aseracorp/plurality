import 'dart:async';
import 'dart:convert';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;

import '../auth/auth-service.dart';
import '../utils/textsafe.dart';
import '../utils/types.dart';
import 'MCP.dart';
import 'api.dart';
import 'client_identity.dart';
import 'client_tool_timeout.dart';
import 'client_tool_validation.dart';
import 'filesystem_service.dart';
import 'shared_preferences_service.dart';
import 'shell_service.dart';
import 'skills_service.dart';

/// Status-frame type carrying a device tool call. Mirrors the server's
/// `StatusEvent.Type` value (docs/device-channel.md).
const String deviceToolRequestType = 'device_tool_request';

/// Skill tool name, matching the chat path's literal.
const String retrieveSkillToolName = 'retrieve_skill';

/// One status-stream frame, as parsed by the device channel.
///
/// `type == ''` is a legacy status event (state / activity / tool_name — the
/// shape ChatService consumes, unchanged by the device-channel work);
/// `type == 'device_tool_request'` carries a device tool call dispatched to
/// THIS client, targeted by the `client_id` on the status-stream connection.
class DeviceToolFrame {
  final String type;
  final String requestId;
  final String conversationId;
  final String toolCallId;
  final String name;
  final String arguments;
  final int timeoutSeconds;
  final String source;

  const DeviceToolFrame({
    this.type = '',
    this.requestId = '',
    this.conversationId = '',
    this.toolCallId = '',
    this.name = '',
    this.arguments = '',
    this.timeoutSeconds = 0,
    this.source = '',
  });

  bool get isDeviceToolRequest => type == deviceToolRequestType;

  /// Parses one decoded status-stream payload. The payload is exactly what
  /// `ApiService.connectStatusStream()` yields: a `Map<String, dynamic>` per
  /// `data:` line, plus the synthetic `{__heartbeat: true}` that the server's
  /// `": ping"` comment lines are mapped to (liveness, not an event).
  ///
  /// Returns null for anything this client must not act on: a legacy status
  /// event (no `type`), a heartbeat, a `device_tool_request` missing its
  /// `request_id` / `device_tool` / `name`, or a malformed payload. Ignoring
  /// legacy events is load-bearing — they are the common case, and treating one
  /// as a device request would dispatch on garbage.
  static DeviceToolFrame? fromStatusEvent(Map<String, dynamic>? json) {
    if (json == null) return null;
    final type = json['type'];
    if (type is! String || type != deviceToolRequestType) return null;

    final requestId = json['request_id'];
    if (requestId is! String || requestId.isEmpty) return null;

    final tool = json['device_tool'];
    if (tool is! Map) return null;

    final name = tool['name'];
    if (name is! String || name.isEmpty) return null;

    // `arguments` is the RAW JSON string of args, per the contract: the args
    // object arrives as a string, not as a parsed map.
    final rawArgs = tool['arguments'];
    final args = rawArgs is String ? rawArgs : '';

    int timeoutSeconds = 0;
    final timeout = tool['timeout_seconds'];
    if (timeout is int) {
      timeoutSeconds = timeout;
    } else if (timeout is num) {
      timeoutSeconds = timeout.toInt();
    } else if (timeout is String) {
      timeoutSeconds = int.tryParse(timeout) ?? 0;
    }

    return DeviceToolFrame(
      type: type,
      requestId: requestId,
      conversationId: tool['conversation_id'] as String? ?? '',
      toolCallId: tool['tool_call_id'] as String? ?? '',
      name: name,
      arguments: args,
      timeoutSeconds: timeoutSeconds,
      source: tool['source'] as String? ?? '',
    );
  }
}

/// One device-tool call this app executed (or refused) unattended.
///
/// Surfaced to the UI because an unattended call must not be invisible: a cron
/// job running PowerShell on the user's machine while they look at a chat is
/// exactly the kind of thing they are entitled to see.
class DeviceActivityEntry {
  final String requestId;
  final String conversationId;
  final String toolCallId;
  final String name;
  final String source;

  /// 'running' while the executor works, then 'done' (result produced and
  /// POSTed) or 'refused' (validated away, never executed).
  String state;

  /// Short human reason for a refusal / failure; empty on success.
  String detail;

  DateTime at;

  DeviceActivityEntry({
    required this.requestId,
    required this.conversationId,
    required this.toolCallId,
    required this.name,
    required this.source,
    this.state = 'running',
    this.detail = '',
    DateTime? at,
  }) : at = at ?? DateTime.now();

  bool get isRunning => state == 'running';

  /// "shell_client__exec" → "shell(exec)", the chat's namespaced-tool rendering.
  /// Duplicated here rather than imported from `utils/index.dart` on purpose:
  /// that file pulls `rootBundle`, `dart:io` and a conditional web import, and
  /// the API layer belongs next to the sockets, not on the UI's asset bundle.
  String get displayTool => formatDeviceToolName(name);

  /// One-line human summary — the banner text and the log line.
  String describe() {
    final who = source.isEmpty ? 'a background run' : source;
    final label = conversationId.isEmpty
        ? who
        : '$who (${ellipsize(conversationId, 12)})';
    switch (state) {
      case 'running':
        return 'Device call running: $displayTool in $label';
      case 'done':
        return 'Device call finished: $displayTool in $label';
      case 'refused':
        return detail.isEmpty
            ? 'Device call refused: $displayTool in $label'
            : 'Device call refused: $displayTool in $label — $detail';
      default:
        return '$displayTool in $label: $state';
    }
  }
}

/// "server__navigate" → "server(navigate)". Non-namespaced names pass through.
String formatDeviceToolName(String name) {
  final idx = name.indexOf('__');
  if (idx < 0) return name;
  return '${name.substring(0, idx)}(${name.substring(idx + 2)})';
}

/// One HTTP call the channel makes. Injected in tests so nothing touches the
/// network; in production it reads the token via `AuthService`, the only place
/// credentials live.
typedef DeviceChannelRequest = Future<Map<String, dynamic>> Function(
  String method,
  String endpoint,
  Map<String, dynamic>? body,
);

/// Shell executor: `ShellService.executeShellExec`.
typedef ShellExec = Future<String> Function(
  String? sandboxRoot,
  Map<String, dynamic> args,
);

/// Filesystem executor; `isRead` selects read vs write.
typedef FsExec = Future<String> Function(
  String? sandboxRoot,
  Map<String, dynamic> args,
  bool isRead,
);

/// Skill executor. Not advertised by the claim (see `DeviceChannel.buildToolDefs`);
/// kept so a claim that ever carries `retrieve_skill` dispatches through the
/// same bounded path as the chat.
typedef SkillExec = Future<String> Function(String skillName, String? fileName);

/// Tool-name constants mirrored here so dispatch does not need the service
/// classes for a name lookup. Keep in sync with `ShellService.toolName` and
/// `FilesystemService.readToolName|writeToolName`.
class ShellServiceName {
  static const String value = 'shell_client__exec';
}

class FilesystemServiceName {
  static const String read = 'filesystem_client__fs_read';
  static const String write = 'filesystem_client__fs_write';
}

/// Default transport: an authenticated request against `AuthService.baseUrl`,
/// same headers as `ApiService.sendPostRequest`. The body is parsed leniently —
/// the server answers `{ok: true, expires_at: ...}` for a claim and `{ok: true}`
/// for a result. A non-2xx becomes an [APIException] carrying the status code,
/// so the caller can tell "404, nobody is waiting for this any more" (which the
/// spec says to ignore) from a real failure.
Future<Map<String, dynamic>> _defaultTransport(
  String method,
  String endpoint,
  Map<String, dynamic>? body,
) async {
  final token = await AuthService().getCurrentUserToken();
  if (token == null) {
    throw APIException('User not authenticated', statusCode: 401);
  }

  final headers = <String, String>{'Authorization': 'Bearer $token'};
  final url = Uri.parse('${AuthService.baseUrl}/$endpoint');

  http.Response response;
  if (method == 'POST') {
    headers['Content-Type'] = 'application/json';
    response = await http.post(
      url,
      headers: headers,
      body: jsonEncode(body ?? const <String, dynamic>{}),
    );
  } else {
    response = await http.get(url, headers: headers);
  }

  Map<String, dynamic> map = <String, dynamic>{};
  if (response.body.isNotEmpty) {
    try {
      final decoded = jsonDecode(response.body);
      if (decoded is Map) map = Map<String, dynamic>.from(decoded);
    } catch (_) {
      map = <String, dynamic>{'body': response.body};
    }
  }

  if (response.statusCode < 200 || response.statusCode >= 300) {
    throw APIException(
      map['error'] as String? ??
          (response.body.isNotEmpty
              ? response.body
              : 'Request failed with status ${response.statusCode}'),
      statusCode: response.statusCode,
    );
  }
  return map;
}

/// The two API calls this channel needs, behind an interface so tests can
/// inject a fake socket and stubbed preferences without touching `ApiService`'s
/// and `SharedPreferencesService`'s singleton state.
class ApiServiceBridge {
  const ApiServiceBridge();

  /// Open the global status stream, tagged with this client's identity so the
  /// server records it on the StatusClient and routes device frames to us. An
  /// empty [clientId] is a legacy connection: statuses only, no device channel
  /// (the server never sends device frames to a legacy client).
  Future<Stream<Map<String, dynamic>>> connectStatusStream({
    http.Client? httpClient,
    String clientId = '',
  }) =>
      ApiService().connectStatusStream(httpClient: httpClient, clientId: clientId);

  /// The app's persisted model selection — the source of the attached folder
  /// path the claim carries.
  Future<ModelSelected> selectedModel() =>
      SharedPreferencesService().getSelectedModel();
}

/// App-wide, always-on device channel (docs/device-channel.md).
///
/// Why this exists: device tools (`shell_client__exec`,
/// `filesystem_client__fs_read|fs_write`, client-side MCP) were reachable only
/// from the app window that streams THAT conversation — they are advertised per
/// request through `ChatPayload.ClientSideTools` and dispatched over that
/// conversation's SSE socket. A background run (cron, webhook `jobs.RunPrompt`,
/// sub-agent `jobs.RunSubAgent`) builds a payload with no client tools and
/// attaches no SSE client, so every device call fast-failed with "cannot be
/// executed by this client" and the model blamed itself for a capability the
/// user's running app had never offered. The app is up and logged in; nothing
/// routed the call to it.
///
/// The channel is deliberately independent of `ChatService`'s status
/// subscription: that one closes after ~45s of silence (its idle watchdog),
/// because the sidebar only needs statuses. If the device channel rode on it, a
/// claim would lapse during a quiet spell and background device calls would
/// fast-fail again — the exact bug this fixes. This connection owns its socket,
/// renews its claim every [claimInterval], and reconnects on backoff.
///
/// What it does:
///  * Claims the tool defs THIS app can execute unattended, sorted by name —
///    the server treats that list as prompt-cache bytes, so the order must be
///    deterministic (an unsorted list reshuffles the system prompt every turn:
///    same class of bug as the disabled-tools-summary fix in #189).
///  * Executes a dispatched call through the SAME bounded executors the chat
///    path uses (`runClientToolBounded*` over ShellService / FilesystemService /
///    MCP), so no process work lands on the UI isolate — the freeze documented
///    in shell_service.dart must not happen here, because a wedged channel also
///    stops renewing the claim.
///  * Refuses any tool the claim does not advertise, and always answers — never
///    drops. Dropping costs the server its full dispatch wait (default 180s)
///    and arms the device-unresponsive memo, so the model reads "the device is
///    dead" where the truth is "this tool is not available here".
///  * Sends output to exactly one place: the result endpoint. Nothing else on
///    this path reaches the network, and no outbound mail/calendar surface is
///    advertised.
///
/// Visibility is non-negotiable: a call made while nobody is looking must not be
/// invisible. Every request publishes a [DeviceActivityEntry] on [activity]
/// (rendered as a banner in the chat) and a debugPrint line at start and at
/// completion. The banner is informational and non-blocking — the run proceeds
/// whether or not anyone reads it, which is what the server's dispatch-in-place
/// model requires.
///
/// The constructor performs no network I/O, so the class is testable: [start]
/// is the only entry that opens a socket.
class DeviceChannel {
  /// Claim TTL sent to the server. The server clamps 15..600 and treats a
  /// lapsed claim as expired (lazy expiry), so background runs fail fast rather
  /// than hang when the app dies.
  static const int ttlSeconds = 120;

  /// Renewal cadence. Must stay comfortably below [ttlSeconds]: a claim is live
  /// only while the socket is open AND renewals arrive, so a cadence near the
  /// TTL leaves a gap on the first missed renewal.
  static const Duration claimInterval = Duration(seconds: 30);

  /// Fallback bound on one dispatched call when the frame carries no
  /// `timeout_seconds`. Matches ChatService's 70s (client_tool_timeout.dart);
  /// the services bound their own syscalls below it.
  static const Duration defaultCallTimeout = Duration(seconds: 70);

  /// How many finished entries [activity] keeps.
  static const int maxActivityEntries = 12;

  /// Status-stream reconnect backoff. The first failure retries at 2s, not
  /// later: a dead socket stops claim renewals, which lapses the claim, which is
  /// the outage this channel exists to prevent.
  static const Duration streamRetryBase = Duration(seconds: 2);
  static const Duration streamRetryMax = Duration(seconds: 60);

  /// Floor on the gap between two claim POSTs. A renewal is one POST, so firing
  /// on every status event would hammer the endpoint; the periodic timer is what
  /// keeps the claim warm.
  static const Duration claimMinGap = Duration(seconds: 5);

  final ApiServiceBridge _api;
  final MCPService _mcp;
  final Future<ShellExec> Function() _shell;
  final Future<FsExec> Function() _fs;
  final Future<SkillExec> Function() _skills;
  final DeviceChannelRequest _transport;
  final http.Client? _httpClient;
  final Duration _claimInterval;
  final Duration _claimMinGap;

  bool _running = false;
  bool _disposed = false;
  int _streamFailures = 0;
  Timer? _streamReconnectTimer;
  Timer? _claimTimer;
  StreamSubscription<Map<String, dynamic>>? _streamSub;
  Future<void>? _claimFlight;
  bool _claimWanted = false;
  DateTime? _lastClaimAt;

  /// request_ids already answered. A status stream replays frames on reconnect,
  /// and re-running a shell command for a request the server already resolved is
  /// a duplicate mutation.
  final Set<String> _answered = <String>{};

  /// Per-conversation serialisation: requests for one conversation run one at a
  /// time, mirroring the chat path's sequential tool loop (the server runs a
  /// device batch sequentially too, and shell work is single-flight on the
  /// device).
  final Map<String, Future<void>> _convTail = <String, Future<void>>{};

  /// Tool defs currently claimed, plus their names in the same sorted order.
  /// Dispatch consults this list — the exact set the server handed the LLM.
  final List<Map<String, dynamic>> _advertised = <Map<String, dynamic>>[];
  final List<String> _advertisedNames = <String>[];

  /// Folder the filesystem tools are sandboxed to, from the app's persisted
  /// model selection. See [buildToolDefs] for why a background run has no folder
  /// of its own.
  String? _attachedFolder;
  bool _attachedFolderKnown = false;

  /// Surfaced to the UI, newest first. Read via [activity]; rebuild on
  /// [activityChanged] (or use ValueListenableBuilder).
  final List<DeviceActivityEntry> _activity = <DeviceActivityEntry>[];
  final ValueNotifier<List<DeviceActivityEntry>> _activityNotifier =
      ValueNotifier<List<DeviceActivityEntry>>(const <DeviceActivityEntry>[]);

  /// Live claim summary for UI / diagnostics: socket state, advertised tools, and
  /// when the server's claim lapses.
  final ValueNotifier<DeviceChannelStatus> _status =
      ValueNotifier<DeviceChannelStatus>(const DeviceChannelStatus());

  /// Side-effect-free: no socket, no POST, no timers. `ClientIdentity().init()`
  /// is a memoised promise already awaited in `main()`, so this touches no
  /// network. All I/O starts in [start] — that is what makes the class testable.
  DeviceChannel({
    ApiServiceBridge? api,
    MCPService? mcp,
    Future<ShellExec> Function()? shellExec,
    Future<FsExec> Function()? fsExec,
    Future<SkillExec> Function()? skillExec,
    DeviceChannelRequest? transport,
    http.Client? httpClient,
    Duration claimInterval = claimInterval,
    Duration claimMinGap = claimMinGap,
  })  : _api = api ?? const ApiServiceBridge(),
        _mcp = mcp ?? MCPService(),
        _shell = shellExec ?? _productionShellExec,
        _fs = fsExec ?? _productionFsExec,
        _skills = skillExec ?? _productionSkillExec,
        _transport = transport ?? _defaultTransport,
        _httpClient = httpClient,
        _claimInterval = claimInterval,
        _claimMinGap = claimMinGap {
    ClientIdentity().init();
  }

  // --- public surface ---

  ValueNotifier<List<DeviceActivityEntry>> get activityChanged =>
      _activityNotifier;
  List<DeviceActivityEntry> get activity =>
      List<DeviceActivityEntry>.unmodifiable(_activity);
  ValueNotifier<DeviceChannelStatus> get statusChanged => _status;
  DeviceChannelStatus get status => _status.value;
  bool get isRunning => _running;
  bool get isDisposed => _disposed;
  int get advertisedToolCount => _advertisedNames.length;
  List<String> get advertisedToolNames =>
      List<String>.unmodifiable(_advertisedNames);

  /// Connect the status stream and claim. Idempotent, and a no-op before a
  /// server URL is configured: a claim POST with no token signs the user out
  /// through the 401 path, so an unauthenticated app must stay silent.
  void start() {
    if (_running || _disposed) return;
    if (!AuthService.isConfigured) return;
    _running = true;
    _streamFailures = 0;
    _connectStream();
    _claimTimer = Timer.periodic(_claimInterval, (_) => _claim());
  }

  /// Drop the socket and the renewal timer. The server drops the claim when its
  /// status client is removed, so a logout stops background runs from routing
  /// device work to this app immediately (otherwise the TTL lapses within
  /// [ttlSeconds]).
  void stop() {
    _running = false;
    _streamReconnectTimer?.cancel();
    _streamReconnectTimer = null;
    _claimTimer?.cancel();
    _claimTimer = null;
    final sub = _streamSub;
    _streamSub = null;
    sub?.cancel();
    _claimWanted = false;
    if (!_disposed) _status.value = _status.value.copyWith(connected: false);
  }

  /// Re-open the socket. Called on app resume / tab visibility: the OS (or a
  /// throttled background tab) can sever a long-lived SSE connection without
  /// firing onError/onDone, which leaves the claim warm server-side while the
  /// app can no longer answer. Cheap when the socket is healthy.
  Future<void> handleResumed() async {
    if (!_running || _disposed) return;
    await _dropStream();
    _streamFailures = 0;
    await _connectStream();
  }

  /// Stop and release the notifiers. App shutdown only — the instance is
  /// unusable afterwards.
  void dispose() {
    if (_disposed) return;
    stop();
    _disposed = true;
    _activity.clear();
    _answered.clear();
    _convTail.clear();
    _advertised.clear();
    _advertisedNames.clear();
    _activityNotifier.dispose();
    _status.dispose();
  }

  /// The defs this app can execute UNATTENDED, sorted by tool name:
  ///  * the device shell — desktop only; `Process.start` does not exist on
  ///    web/mobile, so advertising it there offers the LLM a tool that fails;
  ///  * the sandboxed filesystem read/write pair, when a folder is attached;
  ///  * registered MCP tool defs (already namespaced `server__tool`).
  ///
  /// `retrieve_skill` is deliberately NOT advertised: a background run has no
  /// `available_skills` list to scope it, no conversation to attribute it to,
  /// and it reads app-private files. Advertise the minimum that is useful
  /// unattended — the same posture as the read-only mail bridge.
  ///
  /// A background run attaches no folder, so the sandbox the fs tools execute in
  /// is the claim's folder path, taken from the persisted model selection
  /// ([_attachedFolder]). Without it `executeFsRead` answers "no folder is
  /// attached" and the claim advertises a capability the app cannot fulfil.
  List<Map<String, dynamic>> buildToolDefs({String? attachedFolder}) {
    final folder = (attachedFolder ?? '').isEmpty ? null : attachedFolder;
    final defs = <Map<String, dynamic>>[];
    if (_isDesktop) {
      defs.add(ShellService().getToolDefinition(attachedFolder: folder));
    }
    if (folder != null) defs.addAll(FilesystemService().getToolDefinitions());
    defs.addAll(_mcp.getToolList());
    final sorted = List<Map<String, dynamic>>.of(defs);
    sorted.sort(
      (a, b) => (a['name'] as String? ?? '').compareTo(b['name'] as String? ?? ''),
    );
    return sorted;
  }

  /// The exact claim body defined by docs/device-channel.md. Public so tests pin
  /// the wire shape key-for-key.
  Map<String, dynamic> buildClaimPayload({
    required String clientId,
    required List<Map<String, dynamic>> tools,
    required String? clientFolderPath,
  }) {
    return <String, dynamic>{
      'client_id': clientId,
      'tools': tools,
      'client_folder_path': clientFolderPath ?? '',
      'ttl_seconds': ttlSeconds,
    };
  }

  /// The exact result body defined by docs/device-channel.md:
  /// `{tool_call_id, name, result, error}`. The server concatenates `result` +
  /// `\n` + `error` when both are non-empty, matching how a chat tool result is
  /// stored — so output goes in `result` and a failure in `error`, never smeared
  /// together client-side.
  Map<String, dynamic> buildResultPayload({
    required String toolCallId,
    required String name,
    required String result,
    required String error,
  }) {
    return <String, dynamic>{
      'tool_call_id': toolCallId,
      'name': name,
      'result': result,
      'error': error,
    };
  }

  /// Handle one decoded status-stream frame. Legacy status events (no `type`) and
  /// heartbeats return null and do nothing; a device request returns its parsed
  /// frame and is dispatched.
  ///
  /// Dispatch is fire-and-forget on purpose: awaiting a 70s shell call inside the
  /// stream listener would stall the socket read loop for every frame behind it
  /// (statuses, titles, other device calls).
  Future<DeviceToolFrame?> handleStatusEvent(Map<String, dynamic>? json) async {
    final frame = DeviceToolFrame.fromStatusEvent(json);
    if (frame == null) return null;
    unawaited(processFrame(frame));
    return frame;
  }

  /// Validate, execute, POST — to completion. Public and awaited by tests so one
  /// frame can be driven to its result POST without timers.
  Future<void> processFrame(DeviceToolFrame frame) => _dispatch(frame);

  /// Re-read the attached folder from preferences on the next claim. Called when
  /// the UI attaches/detaches a folder, so the sandbox the fs tools run in
  /// follows the user's choice without waiting out the claim rate limit.
  void invalidateAttachedFolder() => _attachedFolderKnown = false;

  /// Claim now. The in-flight serialisation and [_claimMinGap] rate limit still
  /// apply, so callers can fire this after a folder attach or after MCP
  /// discovery finishes without hammering the endpoint.
  Future<void> refreshClaim() => _claim();

  /// Drop the visible log (the banner's dismiss button). Does not stop the
  /// channel: a dismissed banner must not silence the device.
  void clearActivity() {
    if (_disposed) return;
    _activity.clear();
    _activityNotifier.value = const <DeviceActivityEntry>[];
  }

  // --- claim ---

  Future<String?> _resolveAttachedFolder() async {
    if (!_attachedFolderKnown) {
      try {
        final model = await _api.selectedModel();
        final p = model.clientFolderPath;
        _attachedFolder = (p == null || p.isEmpty) ? null : p;
      } catch (e) {
        debugPrint('[DeviceChannel] attached folder lookup failed: $e');
        _attachedFolder = null;
      }
      _attachedFolderKnown = true;
    }
    return _attachedFolder;
  }

  /// POST /client/claim. Serialised: at most one POST in flight, and a request
  /// arriving while one runs is honoured afterwards — the advertised list can
  /// change mid-flight (MCP discovery finished, a folder was attached), and a
  /// stale claim is a tool the server dispatches and the app refuses.
  Future<void> _claim() async {
    if (!_running || _disposed) return;
    final clientId = ClientIdentity().id;
    if (clientId.isEmpty) {
      debugPrint('[DeviceChannel] identity not resolved — skipping claim');
      return;
    }
    final last = _lastClaimAt;
    if (last != null && DateTime.now().difference(last) < _claimMinGap) {
      _claimWanted = true;
      return;
    }
    if (_claimFlight != null) {
      _claimWanted = true;
      return;
    }

    final folder = await _resolveAttachedFolder();
    final defs = buildToolDefs(attachedFolder: folder);

    _advertised
      ..clear()
      ..addAll(defs);
    _advertisedNames
      ..clear()
      ..addAll(defs.map((d) => (d['name'] as String? ?? '')));

    _claimWanted = false;
    _lastClaimAt = DateTime.now();

    final flight = _postClaim(clientId, defs, folder);
    _claimFlight = flight;
    try {
      await flight;
    } finally {
      if (identical(_claimFlight, flight)) _claimFlight = null;
    }
    if (_running && _claimWanted) await _claim();
  }

  Future<void> _postClaim(
    String clientId,
    List<Map<String, dynamic>> defs,
    String? folder,
  ) async {
    try {
      final res = await _transport(
        'POST',
        'client/claim',
        buildClaimPayload(
          clientId: clientId,
          tools: defs,
          clientFolderPath: folder,
        ),
      );
      DateTime? expiresAt;
      final raw = res['expires_at'];
      if (raw is String && raw.isNotEmpty) {
        try {
          expiresAt = DateTime.parse(raw);
        } catch (_) {
          expiresAt = null;
        }
      }
      _status.value = _status.value.copyWith(
        connected: _running,
        clientId: clientId,
        toolNames: List<String>.of(_advertisedNames),
        clientFolderPath: folder,
        claimExpiresAt: expiresAt,
        lastError: '',
      );
    } on APIException catch (e) {
      if (e.statusCode == 401) {
        debugPrint('[DeviceChannel] Token rejected (401) — stopping channel');
        stop();
        return;
      }
      _noteError('claim: ${e.message}');
    } catch (e) {
      _noteError('claim: $e');
    }
  }

  void _noteError(String message) {
    debugPrint('[DeviceChannel] $message');
    if (_disposed) return;
    _status.value = _status.value.copyWith(
      connected: _running,
      lastError: message,
    );
  }

  // --- status stream ---

  Future<void> _connectStream() async {
    if (!_running || _disposed) return;
    await _dropStream();
    try {
      final stream = await _api.connectStatusStream(
        httpClient: _httpClient,
        clientId: ClientIdentity().id,
      );
      _streamSub = stream.listen(
        (data) {
          _streamFailures = 0;
          if (!_disposed) {
            _status.value = _status.value.copyWith(
              connected: true,
              lastError: '',
            );
          }
          handleStatusEvent(data);
          // The claim goes out as soon as traffic arrives on the socket: a claim
          // is only meaningful while the connection is open, and MCP discovery
          // may still be running at connect time. Later frames are rate-limited
          // by [_claimMinGap]; the periodic timer is what keeps the claim warm.
          _claim();
        },
        onError: (Object e) {
          debugPrint('[DeviceChannel] Status stream error: $e');
          _scheduleReconnect();
        },
        onDone: () {
          debugPrint('[DeviceChannel] Status stream closed');
          _scheduleReconnect();
        },
      );
      if (!_disposed) {
        _status.value = _status.value.copyWith(connected: true, lastError: '');
      }
    } on APIException catch (e) {
      if (e.statusCode == 401) {
        debugPrint('[DeviceChannel] Token rejected (401) — stopping channel');
        stop();
        return;
      }
      _noteError('status stream: ${e.message}');
      _scheduleReconnect();
    } catch (e) {
      _noteError('status stream: $e');
      _scheduleReconnect();
    }
  }

  Future<void> _dropStream() async {
    final sub = _streamSub;
    _streamSub = null;
    if (sub == null) return;
    try {
      await sub.cancel();
    } catch (_) {}
  }

  void _scheduleReconnect() {
    _streamReconnectTimer?.cancel();
    if (!_running || _disposed) return;
    _streamFailures += 1;
    final delay = _backoffFor(_streamFailures);
    _status.value = _status.value.copyWith(
      connected: false,
      lastError: 'status stream down — retrying in ${delay.inSeconds}s',
    );
    _streamReconnectTimer = Timer(delay, () {
      if (_running && !_disposed) _connectStream();
    });
  }

  /// Exponential backoff, 2s → 60s, capped. A channel that gives up silently is
  /// indistinguishable from a device that is offline, and the server cannot tell
  /// the two apart.
  Duration _backoffFor(int failures) {
    final exp = failures < 1 ? 1 : (failures > 6 ? 6 : failures);
    var secs = streamRetryBase.inSeconds * (1 << (exp - 1));
    if (secs > streamRetryMax.inSeconds) secs = streamRetryMax.inSeconds;
    return Duration(seconds: secs);
  }

  // --- dispatch ---

  Future<void> _dispatch(DeviceToolFrame frame) async {
    if (!_running || _disposed) return;
    if (_answered.contains(frame.requestId)) return;
    _answered.add(frame.requestId);

    final entry = DeviceActivityEntry(
      requestId: frame.requestId,
      conversationId: frame.conversationId,
      toolCallId: frame.toolCallId,
      name: frame.name,
      source: frame.source,
    );
    _publish(entry);
    debugPrint('[DeviceChannel] ${entry.describe()}');

    // Serialise per conversation, so a batch of device calls behaves like the
    // chat path's sequential loop.
    final tail = _convTail[frame.conversationId] ?? Future<void>.value();
    final next = tail.then(
      (_) => _run(frame, entry),
      onError: (_) => _run(frame, entry),
    );
    _convTail[frame.conversationId] = next;
    try {
      await next;
    } finally {
      if (identical(_convTail[frame.conversationId], next)) {
        _convTail.remove(frame.conversationId);
      }
    }
  }

  Future<void> _run(DeviceToolFrame frame, DeviceActivityEntry entry) async {
    String result = '';
    // Always empty today: a failure is reported inside `result` the way the
    // chat path reports it (the server concatenates result + "\n" + error, and
    // the model reads `result`). Kept as an explicit field because the wire
    // shape has it and a future split (stderr in `error`) belongs here.
    const String error = '';

    if (_defFor(frame.name) == null) {
      // Never execute a tool the claim does not advertise. Answer, do not drop:
      // dropping costs the server its full dispatch wait and arms the
      // device-unresponsive memo, so the model reads "device dead" where the
      // truth is "not available unattended here".
      result =
          'Error: tool "${frame.name}" is not advertised by this device '
          '(not available unattended on this client). Use a server-side tool '
          '(system_tools__shell_exec, filesystem_server__fs_read) instead.';
      entry.state = 'refused';
      entry.detail = 'tool not advertised';
      debugPrint('[DeviceChannel] refused ${frame.name} (not advertised)');
    } else {
      final toolCall = ToolCall(
        id: frame.toolCallId.isEmpty ? frame.requestId : frame.toolCallId,
        function: FunctionCallData(
          name: frame.name,
          arguments: frame.arguments,
        ),
      );
      // Validate against the schemas THIS claim advertised — the same list the
      // server handed the LLM, so a rejection states a reason the model can act
      // on. Same helper the chat path now uses, so both produce identical text.
      final validationError = validateClientToolArgs(toolCall, _advertised);
      if (validationError != null) {
        result = validationError;
        entry.state = 'refused';
        entry.detail = 'invalid arguments';
        debugPrint('[DeviceChannel] refused ${frame.name}: $validationError');
      } else {
        try {
          result = await _execute(frame, toolCall);
          entry.state = 'done';
        } catch (e) {
          // Same as the chat path: report the real reason verbatim as an error,
          // so the model can fall back to a server-side tool.
          result = 'Error: $e';
          entry.state = 'refused';
          entry.detail = '$e';
          debugPrint('[DeviceChannel] ${frame.name} failed: $e');
        }
      }
    }

    _publish(entry);
    debugPrint('[DeviceChannel] ${entry.describe()}');

    final payload = buildResultPayload(
      toolCallId: frame.toolCallId,
      name: frame.name,
      result: result,
      error: error,
    );
    try {
      await _transport(
        'POST',
        'chat/device-tool-result/${frame.requestId}',
        payload,
      );
    } on APIException catch (e) {
      if (e.statusCode == 404) {
        // The server's pending request already timed out, expired, or belongs to
        // another user. The spec says the app ignores the response; logging is
        // what makes a late result diagnosable.
        debugPrint(
          '[DeviceChannel] result for ${frame.requestId} ignored '
          '(404: request no longer pending)',
        );
      } else {
        _noteError('result POST ${frame.requestId}: ${e.message}');
      }
    } catch (e) {
      _noteError('result POST ${frame.requestId}: $e');
    }
  }

  Map<String, dynamic>? _defFor(String name) {
    for (final d in _advertised) {
      if (d['name'] == name) return d;
    }
    return null;
  }

  /// Execute through the same bounded executors the chat path uses. The outer
  /// bound is the point: services bound their own syscalls, but a service is
  /// more than its syscalls (path_provider, an MCP child spawn), and on a
  /// sleeping Tailscale/SMB share those park. Abandoning the call answers in
  /// bounded time instead of eating the server's dispatch wait, and keeps this
  /// isolate free to keep renewing the claim.
  Future<String> _execute(DeviceToolFrame frame, ToolCall toolCall) async {
    final timeout = frame.timeoutSeconds > 0
        ? Duration(seconds: frame.timeoutSeconds)
        : defaultCallTimeout;
    final name = frame.name;

    if (name == ShellServiceName.value) {
      final root = _attachedFolder;
      final exec = await _shell();
      return runClientToolBounded(
        name,
        () => exec(root, _argsOf(toolCall)),
        timeout: timeout,
      );
    }

    if (name == FilesystemServiceName.read ||
        name == FilesystemServiceName.write) {
      final root = _attachedFolder;
      final isRead = name == FilesystemServiceName.read;
      final exec = await _fs();
      return runClientToolBounded(
        name,
        () => exec(root, _argsOf(toolCall), isRead),
        timeout: timeout,
      );
    }

    if (name == retrieveSkillToolName) {
      final args = _argsOf(toolCall);
      final exec = await _skills();
      return runClientToolBounded(
        name,
        () => exec(
          args['skill_name'] as String? ?? '',
          args['file_name'] as String?,
        ),
        timeout: timeout,
      );
    }

    final serverName = _mcp.getToolServerName(name);
    if (serverName == null) {
      // Defensive: the advertised-name check above refuses anything this client
      // cannot dispatch, so arriving here means the claim list changed
      // mid-flight (MCP re-discovery). Answer, don't execute.
      return 'Error: MCP server not found for tool $name';
    }
    return runClientToolBoundedJson(
      name,
      () => _mcp.serverManager.sendRequest(
        serverName,
        'tools/call',
        {
          'name': MCPService.bareToolName(name),
          'arguments': _argsJsonOf(toolCall),
        },
        frame.conversationId,
      ),
      timeout: timeout,
    );
  }

  Map<String, dynamic> _argsOf(ToolCall toolCall) {
    final decoded = _argsJsonOf(toolCall);
    if (decoded is Map) return Map<String, dynamic>.from(decoded);
    return <String, dynamic>{};
  }

  dynamic _argsJsonOf(ToolCall toolCall) {
    final raw = toolCall.function.arguments.isEmpty
        ? '{}'
        : toolCall.function.arguments;
    final decoded = jsonDecode(raw);
    if (decoded is Map) return Map<String, dynamic>.from(decoded);
    return decoded;
  }

  // --- UI surfacing ---

  void _publish(DeviceActivityEntry entry) {
    if (_disposed) return;
    final next = List<DeviceActivityEntry>.of(_activity);
    final idx = next.indexWhere((e) => e.requestId == entry.requestId);
    if (idx >= 0) {
      next[idx] = entry;
    } else {
      next.insert(0, entry);
      if (next.length > maxActivityEntries) {
        next.removeRange(maxActivityEntries, next.length);
      }
    }
    _activity
      ..clear()
      ..addAll(next);
    // Assign a fresh list so the notifier fires: entries are mutable, and
    // reassigning the same list instance notifies nobody.
    _activityNotifier.value = List<DeviceActivityEntry>.unmodifiable(next);
  }

  static Future<ShellExec> _productionShellExec() async =>
      ShellService().executeShellExec;

  static Future<FsExec> _productionFsExec() async =>
      (String? root, Map<String, dynamic> args, bool isRead) => isRead
          ? FilesystemService().executeFsRead(root, args)
          : FilesystemService().executeFsWrite(root, args);

  static Future<SkillExec> _productionSkillExec() async =>
      (String skillName, String? fileName) =>
          SkillsService().executeRetrieveSkill(skillName, fileName);

  /// True on the three platforms where `Process.start` works — the same gate
  /// ChatService uses to decide whether the device shell is visible to the LLM.
  static bool get _isDesktop =>
      !kIsWeb && (Platform.isWindows || Platform.isMacOS || Platform.isLinux);
}

/// Live channel state for UI / diagnostics.
class DeviceChannelStatus {
  final bool connected;
  final String clientId;
  final List<String> toolNames;
  final String? clientFolderPath;

  /// When the server's claim lapses (null = never claimed). Surfaced so a UI can
  /// say "device channel idle — background runs cannot use this device".
  final DateTime? claimExpiresAt;
  final String lastError;

  const DeviceChannelStatus({
    this.connected = false,
    this.clientId = '',
    this.toolNames = const [],
    this.clientFolderPath,
    this.claimExpiresAt,
    this.lastError = '',
  });

  bool get hasClaim => claimExpiresAt != null;

  DeviceChannelStatus copyWith({
    bool? connected,
    String? clientId,
    List<String>? toolNames,
    Object? clientFolderPath = _unsetStatus,
    Object? claimExpiresAt = _unsetStatus,
    String? lastError,
  }) {
    return DeviceChannelStatus(
      connected: connected ?? this.connected,
      clientId: clientId ?? this.clientId,
      toolNames: toolNames ?? this.toolNames,
      clientFolderPath: identical(clientFolderPath, _unsetStatus)
          ? this.clientFolderPath
          : clientFolderPath as String?,
      claimExpiresAt: identical(claimExpiresAt, _unsetStatus)
          ? this.claimExpiresAt
          : claimExpiresAt as DateTime?,
      lastError: lastError ?? this.lastError,
    );
  }
}

/// Sentinel so `copyWith` can distinguish "leave unchanged" from "clear" —
/// the same pattern `ModelSelected.copyWith` uses for clientFolderPath.
const Object _unsetStatus = Object();

/// Singleton accessor, mirroring the app's service pattern (ChatService,
/// ShellService, …) so widgets can read status without threading an instance
/// through the widget tree. Tests construct [DeviceChannel] directly with an
/// injected transport and never touch this.
class DeviceChannelService {
  static final DeviceChannelService _instance = DeviceChannelService._internal();
  factory DeviceChannelService() => _instance;
  DeviceChannelService._internal();

  DeviceChannel? _channel;

  /// The live channel, or null before [ensureStarted]. Widgets read
  /// `channel?.statusChanged` / `channel?.activityChanged` and render nothing
  /// when it is absent.
  DeviceChannel? get channel => _channel;

  /// Start the channel when authenticated. Safe to call repeatedly (from
  /// `build()`, like `ChatService().ensureConnected()`): [DeviceChannel.start]
  /// is idempotent, and a no-op when no server URL is configured.
  DeviceChannel ensureStarted() {
    final existing = _channel;
    if (existing != null && !existing.isDisposed) {
      if (!existing.isRunning) existing.start();
      return existing;
    }
    final created = DeviceChannel();
    _channel = created;
    created.start();
    return created;
  }

  /// Tear down on logout: drops the socket, so the server drops the claim and
  /// background runs stop routing device work to this app.
  void stopAndDispose() {
    final c = _channel;
    _channel = null;
    c?.dispose();
  }

  /// App resume: re-open a socket the OS severed while backgrounded.
  Future<void> handleResumed() async {
    final c = _channel;
    if (c != null && !c.isDisposed) await c.handleResumed();
  }
}
