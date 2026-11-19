import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:shared_preferences/shared_preferences.dart';

import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/client_identity.dart';
import 'package:plurality/api/device_channel.dart';
import 'package:plurality/auth/auth-service.dart';
import 'package:plurality/utils/types.dart';

/// The device channel (docs/device-channel.md) is the app-wide route that lets
/// a BACKGROUND run — cron, webhook, sub-agent — use device tools. Before it,
/// device tools were advertised only by the app window streaming THAT
/// conversation, so `jobs.RunPrompt` fast-failed every `shell_client__exec`
/// with "cannot be executed by this client" and the model blamed itself for a
/// capability the user's running app had never offered.
///
/// These tests pin the wire contract with the Go side, because that shape is
/// the whole interface: the claim body (exact keys, tool names SORTED — the
/// server treats the list as prompt-cache bytes), the `device_tool_request`
/// frame, the refusal of anything the claim does not advertise, the validation
/// error, and the result POST.
///
/// Nothing touches the network: the transport is an injected fake and the
/// status stream is a broadcast controller, so the socket, the timers and the
/// executors are all stubbed.
class _Call {
  final String method;
  final String endpoint;
  final Map<String, dynamic>? body;
  _Call(this.method, this.endpoint, this.body);
}

class _FakeTransport {
  final List<_Call> calls = [];
  Map<String, dynamic> claimResponse = const {'ok': true};
  Map<String, dynamic> resultResponse = const {'ok': true};
  APIException? claimError;

  /// Thrown for `chat/device-tool-result/*` POSTs. A 404 means the server's
  /// pending request already timed out; the spec says the app ignores it.
  APIException? resultError;

  Future<Map<String, dynamic>> handle(
    String method,
    String endpoint,
    Map<String, dynamic>? body,
  ) async {
    calls.add(_Call(method, endpoint, body));
    if (endpoint == 'client/claim') {
      final e = claimError;
      if (e != null) throw e;
      return claimResponse;
    }
    final e = resultError;
    if (e != null) throw e;
    return resultResponse;
  }

  int count(String endpoint) =>
      calls.where((c) => c.endpoint == endpoint).length;

  _Call? last(String endpoint) {
    final list = calls.where((c) => c.endpoint == endpoint).toList();
    return list.isEmpty ? null : list.last;
  }
}

class _FakeBridge extends ApiServiceBridge {
  _FakeBridge({this.folder});

  /// The folder the app has attached — what the claim advertises as
  /// client_folder_path and what the fs tools are sandboxed to.
  final String? folder;

  int connectCount = 0;
  String? lastClientId;
  final StreamController<Map<String, dynamic>> controller =
      StreamController<Map<String, dynamic>>.broadcast();

  @override
  Future<Stream<Map<String, dynamic>>> connectStatusStream({
    http.Client? httpClient,
    String clientId = '',
  }) async {
    connectCount++;
    lastClientId = clientId;
    return controller.stream;
  }

  @override
  Future<ModelSelected> selectedModel() async =>
      ModelSelected(clientFolderPath: folder);
}

/// Recording executors, so "never executed" is directly assertable.
class _FakeExecutors {
  int shellCalls = 0;
  int fsCalls = 0;
  int skillCalls = 0;
  String? lastRoot;
  Map<String, dynamic>? lastArgs;
  bool lastIsRead = true;

  /// Set to a never-completing future to model a wedged syscall.
  Future<String> Function()? shellOverride;
  String shellResult = 'shell stdout';
  String fsResult = 'fs result';

  /// Number of result POSTs observed at the moment each shell call started.
  /// Serialisation is proven by ordering, not by a live counter: a call
  /// abandoned by its bound never decrements one, so the counter would report
  /// phantom concurrency.
  final List<int> postsAtStart = [];
  int Function()? postCountProbe;

  Future<ShellExec> shell() async =>
      (String? root, Map<String, dynamic> args) async {
        shellCalls++;
        lastRoot = root;
        lastArgs = args;
        postsAtStart.add(postCountProbe?.call() ?? 0);
        final override = shellOverride;
        return override != null ? await override() : shellResult;
      };

  Future<FsExec> fs() async =>
      (String? root, Map<String, dynamic> args, bool isRead) async {
        fsCalls++;
        lastRoot = root;
        lastArgs = args;
        lastIsRead = isRead;
        return fsResult;
      };

  Future<SkillExec> skills() async =>
      (String skillName, String? fileName) async {
        skillCalls++;
        return 'skill body';
      };
}

Map<String, dynamic> _mcpDef(String name, {List<String> required = const []}) {
  return <String, dynamic>{
    'name': name,
    'description': '[MCP server: x] $name',
    'parameters': <String, dynamic>{
      'type': 'object',
      'properties': <String, dynamic>{
        'text': <String, dynamic>{'type': 'string'},
        'box': <String, dynamic>{'type': 'string'},
      },
      if (required.isNotEmpty) 'required': required,
    },
  };
}

Map<String, dynamic> _deviceFrame({
  String requestId = 'req_1',
  String name = 'shell_client__exec',
  String args = '{"command":"ls"}',
  String conversationId = 'conv_bg',
  String toolCallId = 'call_1',
  String source = 'background',
  int timeoutSeconds = 5,
}) {
  return <String, dynamic>{
    'type': 'device_tool_request',
    'request_id': requestId,
    'device_tool': <String, dynamic>{
      'conversation_id': conversationId,
      'tool_call_id': toolCallId,
      'name': name,
      // Per the contract: the raw JSON string of args, not a parsed object.
      'arguments': args,
      'timeout_seconds': timeoutSeconds,
      'source': source,
    },
  };
}

DeviceChannel _channel({
  required _FakeTransport transport,
  required _FakeBridge bridge,
  required _FakeExecutors executors,
  List<Map<String, dynamic>> mcpTools = const [],
  Map<String, String> mcpServerNames = const {},
}) {
  final mcp = MCPService();
  mcp.toolLists = List<dynamic>.of(mcpTools);
  mcp.toolServerNames = Map<String, String>.from(mcpServerNames);
  return DeviceChannel(
    api: bridge,
    mcp: mcp,
    transport: transport.handle,
    shellExec: executors.shell,
    fsExec: executors.fs,
    skillExec: executors.skills,
    // Tiny intervals: the 30s/5s production values are covered by the
    // constants, not by waiting on them in a test.
    claimInterval: const Duration(milliseconds: 40),
    claimMinGap: const Duration(milliseconds: 1),
  );
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  late _FakeTransport transport;
  late _FakeBridge bridge;
  late _FakeExecutors executors;
  late DeviceChannel channel;

  Future<void> settle() => pumpEventQueue();

  setUpAll(() async {
    SharedPreferences.setMockInitialValues({});
    // Native builds read the server URL from SharedPreferences; identity is the
    // local hostname on desktop. Pin both so the assertions are deterministic.
    await AuthService.setServerUrl('http://127.0.0.1:9');
    ClientIdentity().resetForTest();
  });

  setUp(() async {
    // Identity is the local hostname on desktop and is read by the claim body;
    // resetForTest() clears it, so re-init before every case.
    await ClientIdentity().init();
    transport = _FakeTransport();
    executors = _FakeExecutors();
    bridge = _FakeBridge(folder: r'C:\Code\Outlook');
    channel = _channel(
      transport: transport,
      bridge: bridge,
      executors: executors,
      mcpTools: [_mcpDef('mail__mail_list', required: ['box'])],
      mcpServerNames: {'mail__mail_list': 'mail'},
    );
  });

  tearDown(() {
    channel.dispose();
    bridge.controller.close();
    ClientIdentity().resetForTest();
  });

  group('constructor is side-effect free', () {
    test('no socket, no claim POST before start()', () {
      // The whole point of keeping network out of the constructor: the class is
      // testable, and an app that is not authenticated must stay silent.
      final t = _FakeTransport();
      final b = _FakeBridge(folder: null);
      final c = _channel(
        transport: t,
        bridge: b,
        executors: _FakeExecutors(),
      );
      expect(c.isRunning, isFalse);
      expect(b.connectCount, 0);
      expect(t.calls, isEmpty);
      c.dispose();
    });
  });

  group('claim', () {
    test('POSTs /client/claim with the exact spec body and sorted tool names', () async {
      channel.start();
      await settle();

      final call = transport.last('client/claim');
      expect(call, isNotNull, reason: 'a claim must be posted on connect');
      expect(call!.method, 'POST');

      final body = call.body!;
      expect(body.keys.toList()..sort(), [
        'client_folder_path',
        'client_id',
        'tools',
        'ttl_seconds',
      ]);
      expect(body['client_id'], ClientIdentity().id);
      expect(body['client_folder_path'], r'C:\Code\Outlook');
      expect(body['ttl_seconds'], 120);

      final tools = body['tools'] as List<dynamic>;
      final names = tools
          .map((t) => (t as Map<String, dynamic>)['name'] as String)
          .toList();
      // Deterministic order: the server keys its prompt cache on these bytes, so
      // an unordered claim would reshuffle the system prompt on every renewal.
      expect(names, isOrderedAscending);
      expect(names, contains('shell_client__exec'));
      expect(names, contains('filesystem_client__fs_read'));
      expect(names, contains('filesystem_client__fs_write'));
      expect(names, contains('mail__mail_list'));
      expect(names, isNot(contains('retrieve_skill')),
          reason: 'skills are app-private and unscoped for a background run');
    });

    test('no folder attached → no filesystem tools, empty folder path', () async {
      bridge = _FakeBridge(folder: null);
      channel = _channel(
        transport: transport,
        bridge: bridge,
        executors: executors,
      );
      channel.start();
      await settle();

      final body = transport.last('client/claim')!.body!;
      expect(body['client_folder_path'], '');
      final names = (body['tools'] as List<dynamic>)
          .map((t) => (t as Map<String, dynamic>)['name'] as String)
          .toList();
      expect(names, isNot(contains('filesystem_client__fs_read')));
      expect(names, isNot(contains('filesystem_client__fs_write')));
      channel.dispose();
    });

    test('the status stream is opened with this client_id', () async {
      channel.start();
      await settle();
      expect(bridge.lastClientId, ClientIdentity().id,
          reason: 'device frames are targeted: a client_id-less connection is '
              'legacy and never receives them');
      expect(bridge.connectCount, 1);
    });

    test('claim is renewed on the periodic cadence', () async {
      channel.start();
      await settle();
      expect(transport.count('client/claim'), 1);

      await Future<void>.delayed(const Duration(milliseconds: 120));
      expect(transport.count('client/claim'), greaterThan(1),
          reason: 'the claim lives on TTL renewal; a claim that stops being '
              'renewed is treated as expired and background runs fail fast');
    });

    test('claim body is byte-stable across renewals', () async {
      channel.start();
      await settle();
      final first = transport.last('client/claim')!.body!;
      await channel.refreshClaim();
      final second = transport.last('client/claim')!.body!;
      expect(first['tools'], second['tools']);
      expect(first['client_folder_path'], second['client_folder_path']);
    });

    test('a 401 on the claim stops the channel', () async {
      transport.claimError = APIException('no token', statusCode: 401);
      channel.start();
      await settle();
      expect(channel.isRunning, isFalse,
          reason: 'a rejected token means signed out; retrying hammers the '
              'server with a stale credential');
    });

    test('a transient claim failure keeps the channel alive', () async {
      transport.claimError = APIException('boom', statusCode: 500);
      channel.start();
      await settle();
      expect(channel.isRunning, isTrue);
      expect(channel.status.lastError, contains('boom'));
      transport.claimError = null;
      await channel.refreshClaim();
      expect(channel.status.lastError, isEmpty);
    });

    test('start() records the server-reported claim expiry', () async {
      transport.claimResponse = const {
        'ok': true,
        'expires_at': '2026-11-19T12:00:00Z',
      };
      channel.start();
      await settle();
      expect(channel.status.hasClaim, isTrue);
      expect(channel.status.claimExpiresAt, DateTime.parse('2026-11-19T12:00:00Z'));
      expect(channel.status.toolNames, contains('shell_client__exec'));
    });
  });

  group('device_tool_request frame parsing', () {
    test('parses the spec frame shape', () {
      final frame = DeviceToolFrame.fromStatusEvent(_deviceFrame())!;
      expect(frame.isDeviceToolRequest, isTrue);
      expect(frame.requestId, 'req_1');
      expect(frame.conversationId, 'conv_bg');
      expect(frame.toolCallId, 'call_1');
      expect(frame.name, 'shell_client__exec');
      expect(frame.arguments, '{"command":"ls"}');
      expect(frame.timeoutSeconds, 5);
      expect(frame.source, 'background');
    });

    test('a legacy status event (no type field) is ignored', () {
      // The common frame on this stream. Treating it as a device request would
      // dispatch on garbage.
      const legacy = <String, dynamic>{
        'conversation_id': 'conv_1',
        'state': 'processing',
        'activity': 'typing',
        'tool_name': '',
      };
      expect(DeviceToolFrame.fromStatusEvent(legacy), isNull);
    });

    test('the server heartbeat is ignored', () {
      expect(
        DeviceToolFrame.fromStatusEvent(
          const <String, dynamic>{'__heartbeat': true},
        ),
        isNull,
      );
    });

    test('a device request missing request_id / device_tool / name is ignored', () {
      expect(
        DeviceToolFrame.fromStatusEvent(<String, dynamic>{
          'type': 'device_tool_request',
        }),
        isNull,
      );
      expect(
        DeviceToolFrame.fromStatusEvent(<String, dynamic>{
          'type': 'device_tool_request',
          'request_id': 'req_x',
        }),
        isNull,
      );
      expect(
        DeviceToolFrame.fromStatusEvent(<String, dynamic>{
          'type': 'device_tool_request',
          'request_id': 'req_x',
          'device_tool': <String, dynamic>{'tool_call_id': 'c'},
        }),
        isNull,
      );
    });

    test('a legacy status event on the live stream executes nothing', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      final parsed = await channel.handleStatusEvent(<String, dynamic>{
        'conversation_id': 'conv_1',
        'state': 'processing',
        'activity': 'tool_use',
        'tool_name': 'shell_client__exec',
      });
      await settle();

      expect(parsed, isNull);
      expect(executors.shellCalls, 0,
          reason: 'a status event saying "tool_use" is a display event, not a '
              'dispatch — ChatService already consumes it for the sidebar');
      expect(transport.calls, isEmpty);
      expect(channel.activity, isEmpty);
    });

    test('a heartbeat on the live stream executes nothing', () async {
      channel.start();
      await settle();
      transport.calls.clear();
      final parsed = await channel.handleStatusEvent(
        const <String, dynamic>{'__heartbeat': true},
      );
      await settle();
      expect(parsed, isNull);
      expect(executors.shellCalls, 0);
    });
  });

  group('dispatch', () {
    test('an advertised shell call executes through the bounded executor and POSTs the result', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(_deviceFrame())!,
      );

      expect(executors.shellCalls, 1);
      expect(executors.lastArgs, {'command': 'ls'});
      // The sandbox the fs/shell tools run in is the claim's folder: a
      // background run has no folder of its own.
      expect(executors.lastRoot, r'C:\Code\Outlook');

      final call = transport.last('chat/device-tool-result/req_1');
      expect(call, isNotNull, reason: 'a request must always be answered, never dropped');
      expect(call!.method, 'POST');
      final body = call.body!;
      expect(body.keys.toList()..sort(), ['error', 'name', 'result', 'tool_call_id']);
      expect(body['tool_call_id'], 'call_1');
      expect(body['name'], 'shell_client__exec');
      expect(body['result'], 'shell stdout');
      expect(body['error'], '');
    });

    test('a tool the claim does not advertise is refused, with an error result and no execution', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(name: 'filesystem_server__fs_write'),
        )!,
      );

      expect(executors.shellCalls, 0);
      expect(executors.fsCalls, 0,
          reason: 'the whole point: a background run cannot reach a tool this '
              'app never advertised, and the write tool is a mutation');
      final body = transport.last('chat/device-tool-result/req_1')!.body!;
      expect(body['result'], contains('not advertised'));
      expect(body['result'], contains('filesystem_server__fs_write'));
      expect(body['result'], contains('system_tools__shell_exec'),
          reason: 'the model needs a fallback it can actually use');
      expect(body['error'], '');

      final entry = channel.activity.first;
      expect(entry.state, 'refused');
      expect(entry.detail, 'tool not advertised');
    });

    test('args validation failure produces the validation error result, no execution', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(args: '{"command":"ls","pwdx":"/tmp"}'),
        )!,
      );

      expect(executors.shellCalls, 0);
      final body = transport.last('chat/device-tool-result/req_1')!.body!;
      expect(body['result'], contains('invalid arguments for shell_client__exec'));
      expect(body['result'], contains('unknown parameter(s) [pwdx]'));
      expect(body['result'], contains('allowed parameters: [command, operation, pwd, tail_bytes, task_id]'));
      expect(channel.activity.first.state, 'refused');
      expect(channel.activity.first.detail, 'invalid arguments');
    });

    test('missing required args are refused against the advertised schema', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(
            name: 'filesystem_client__fs_read',
            args: '{"op":"read"}',
            toolCallId: 'call_fs',
          ),
        )!,
      );

      expect(executors.fsCalls, 0);
      final body = transport.last('chat/device-tool-result/req_1')!.body!;
      expect(body['tool_call_id'], 'call_fs');
      expect(body['name'], 'filesystem_client__fs_read');
      expect(body['result'], contains('missing required parameter(s) [path]'));
    });

    test('a filesystem write call routes to the write executor', () async {
      channel.start();
      await settle();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(
            name: 'filesystem_client__fs_write',
            args: '{"op":"mkdir","path":"out"}',
          ),
        )!,
      );

      expect(executors.fsCalls, 1);
      expect(executors.lastIsRead, isFalse);
      expect(transport.last('chat/device-tool-result/req_1')!.body['result'], 'fs result');
    });

    test('an advertised MCP call dispatches through the MCP manager and answers with an error result when its server is not running', () async {
      channel.start();
      await settle();

      // The MCP manager has no registered process in a test, so sendRequest
      // throws. The channel must turn that into an 'Error: ...' result, not a
      // dropped request.
      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(name: 'mail__mail_list', args: '{"box":"info"}'),
        )!,
      );

      final body = transport.last('chat/device-tool-result/req_1')!.body!;
      expect(body['result'], startsWith('Error:'));
      expect(body['result'], contains('mail'));
    });

    test('requests for the same conversation run sequentially', () async {
      channel.start();
      await settle();

      // The first call wedges past the frame's timeout; the second must not
      // start on top of it (shell work is single-flight on the device, and the
      // server runs its device batch sequentially too).
      executors.shellOverride = () => Completer<String>().future;
      executors.postCountProbe = () =>
          transport.count('chat/device-tool-result/req_a');

      final first = channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(requestId: 'req_a', timeoutSeconds: 1),
        )!,
      );
      final second = channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(requestId: 'req_b', timeoutSeconds: 1),
        )!,
      );

      await Future.wait([first, second]);

      expect(executors.shellCalls, 2);
      // Call 2 started only after call 1's result was POSTed: same-conversation
      // requests are serialised, never stacked.
      expect(executors.postsAtStart, [0, 1],
          reason: 'serialised per conversation, like the chat path\'s '
              'sequential tool loop');
      final a = transport.last('chat/device-tool-result/req_a')!.body!;
      expect(a['result'], contains('did not finish within 1s'),
          reason: 'a wedged call answers with the bound, not a hang');
      expect(
        transport.last('chat/device-tool-result/req_b')!.body!['result'],
        'shell stdout',
      );
    });

    test('a replayed request_id is answered once', () async {
      channel.start();
      await settle();
      transport.calls.clear();

      final frame = DeviceToolFrame.fromStatusEvent(_deviceFrame())!;
      await channel.processFrame(frame);
      await channel.processFrame(frame);

      expect(executors.shellCalls, 1,
          reason: 'a status stream replays frames on reconnect; re-running a '
              'shell command is a duplicate mutation');
      expect(transport.count('chat/device-tool-result/req_1'), 1);
    });

    test('a frame is ignored after stop()', () async {
      channel.start();
      await settle();
      channel.stop();
      transport.calls.clear();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(_deviceFrame())!,
      );
      expect(executors.shellCalls, 0);
      expect(transport.calls, isEmpty,
          reason: 'after logout the app must not act on a routed request');
    });

    test('a 404 result response is ignored, not retried', () async {
      channel.start();
      await settle();
      transport.resultError = APIException('gone', statusCode: 404);

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(_deviceFrame())!,
      );

      expect(executors.shellCalls, 1);
      expect(transport.count('chat/device-tool-result/req_1'), 1,
          reason: 'the server already timed out — re-sending is pointless and '
              'the spec says to ignore it');
      expect(channel.isRunning, isTrue);
    });
  });

  group('unattended calls are visible to the human', () {
    test('every request publishes an entry: running, then finished', () async {
      channel.start();
      await settle();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(_deviceFrame())!,
      );

      expect(channel.activity, hasLength(1));
      final entry = channel.activity.first;
      expect(entry.state, 'done');
      expect(entry.name, 'shell_client__exec');
      expect(entry.source, 'background');
      expect(entry.conversationId, 'conv_bg');
      expect(entry.describe(), contains('shell(exec)'),
          reason: 'displayed like the chat\'s namespaced tool rendering');
      expect(entry.describe(), contains('conv_bg'));
    });

    test('the notifier fires on publish and on completion (banner rebuilds)', () async {
      channel.start();
      await settle();

      final seen = <int>[];
      void listener() => seen.add(channel.activity.length);
      channel.activityChanged.addListener(listener);

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(_deviceFrame())!,
      );
      channel.activityChanged.removeListener(listener);

      // Two notifications: the 'running' entry and the same entry flipped to
      // 'done'. A banner that only learned at the end would hide an in-flight
      // command on the user's machine.
      expect(seen.length, greaterThanOrEqualTo(2));
      expect(seen.first, 1);
    });

    test('a refusal is visible with its reason', () async {
      channel.start();
      await settle();

      await channel.processFrame(
        DeviceToolFrame.fromStatusEvent(
          _deviceFrame(name: 'filesystem_server__fs_write'),
        )!,
      );

      expect(channel.activity.first.describe(), contains('refused'));
      expect(channel.activity.first.describe(), contains('tool not advertised'));
    });

    test('newest first, capped, and dismissable', () async {
      channel.start();
      await settle();

      for (var i = 0; i < DeviceChannel.maxActivityEntries + 3; i++) {
        await channel.processFrame(
          DeviceToolFrame.fromStatusEvent(
            _deviceFrame(requestId: 'req_$i', toolCallId: 'call_$i'),
          )!,
        );
      }
      expect(channel.activity, hasLength(DeviceChannel.maxActivityEntries));
      expect(channel.activity.first.requestId, 'req_${DeviceChannel.maxActivityEntries + 2}',
          reason: 'newest first');

      channel.clearActivity();
      expect(channel.activity, isEmpty);
      expect(channel.isRunning, isTrue,
          reason: 'dismissing the log must not silence the device');
    });
  });

  group('lifecycle', () {
    test('stop() drops the socket and the renewal timer', () async {
      channel.start();
      await settle();

      channel.stop();
      transport.calls.clear();
      await Future<void>.delayed(const Duration(milliseconds: 120));

      expect(channel.isRunning, isFalse);
      expect(transport.count('client/claim'), 0,
          reason: 'a stopped channel must keep renewing a claim the server '
              'will route work to');
      expect(bridge.connectCount, 1, reason: 'no reconnect loop after stop');
      expect(channel.status.connected, isFalse);
    });

    test('handleResumed() re-opens the socket', () async {
      channel.start();
      await settle();
      expect(bridge.connectCount, 1);

      await channel.handleResumed();
      await settle();
      expect(bridge.connectCount, 2,
          reason: 'the OS / a throttled tab severs long-lived SSE sockets; the '
              'claim dies with the connection, so resume must re-open it');
      expect(channel.isRunning, isTrue);
    });

    test('handleResumed() is a no-op before start / after stop', () async {
      await channel.handleResumed();
      expect(bridge.connectCount, 0);

      channel.start();
      await settle();
      channel.stop();
      final before = bridge.connectCount;
      await channel.handleResumed();
      expect(bridge.connectCount, before,
          reason: 'a logged-out app must not reopen a socket');
    });

    test('start() is idempotent', () async {
      channel.start();
      channel.start();
      await settle();
      expect(bridge.connectCount, 1);
    });

    test('start() is silent with no server URL configured', () async {
      final saved = AuthService.nativeServerUrl;
      await AuthService.setServerUrl('');
      final t = _FakeTransport();
      final b = _FakeBridge(folder: null);
      final c = _channel(
        transport: t,
        bridge: b,
        executors: _FakeExecutors(),
      );
      c.start();
      await settle();
      expect(c.isRunning, isFalse,
          reason: 'a claim POST with no token signs the user out through the '
              '401 path');
      expect(b.connectCount, 0);
      expect(t.calls, isEmpty);
      c.dispose();
      await AuthService.setServerUrl(saved);
    });

    test('the stream is reconnected after it closes', () async {
      channel.start();
      await settle();
      expect(bridge.connectCount, 1);

      await bridge.controller.close();
      // Backoff starts at 2s (streamRetryBase): a dead socket stops claim
      // renewals, so the first retry is fast, then it backs off toward 60s.
      await Future<void>.delayed(const Duration(seconds: 3));
      expect(bridge.connectCount, greaterThanOrEqualTo(2));
    });

    test('dispose() is safe to call twice and clears state', () async {
      channel.start();
      await settle();
      channel.dispose();
      channel.dispose();
      expect(channel.isDisposed, isTrue);
      expect(channel.activity, isEmpty);
    });
  });

  group('spec constants', () {
    test('ttl and renewal cadence leave room for a missed renewal', () {
      expect(DeviceChannel.ttlSeconds, 120, reason: 'docs/device-channel.md');
      expect(DeviceChannel.claimInterval, const Duration(seconds: 30));
      expect(
        DeviceChannel.claimInterval.inSeconds,
        lessThan(DeviceChannel.ttlSeconds ~/ 2),
        reason: 'a cadence near the TTL leaves a gap on the first missed '
            'renewal, and an expired claim makes background runs fail fast',
      );
    });

    test('the per-call bound stays below the server\'s dispatch wait', () {
      expect(DeviceChannel.defaultCallTimeout.inSeconds, lessThan(180),
          reason: 'the server waits ~180s for a device result; the app must '
              'answer with a real timeout message, not a silent hang');
    });
  });
}
