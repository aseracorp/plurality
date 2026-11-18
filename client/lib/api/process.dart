import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:uuid/uuid.dart';

import 'shell_run.dart';

class ProcessManager {
  final String command;
  final String name;
  final List<String> args;

  Process? _process;
  final _responseCompleters = <String, Completer<dynamic>>{};
  final _uuid = Uuid();
  final _processOutput = StreamController<String>.broadcast();
  bool _isRunning = false;
  bool _isClosed = false;

  Stream<String> get outputStream => _processOutput.stream;
  bool get isRunning => _isRunning;

  /// Bounds on starting the MCP child process.
  ///
  /// `start()` is awaited from the UI isolate: `initMCP()` runs it for every
  /// configured server at app startup, and `_getConversationServer()` runs it
  /// on the tool-dispatch path for stateful servers. Two calls there were
  /// unbounded on Windows:
  ///
  ///  * `Process.run('where', [cmd])` — spawns `where.exe` and waits for it.
  ///    Held by endpoint security / group policy it never returns.
  ///  * `Process.start` itself — a binary on a Tailscale/SMB/OneDrive path
  ///    blocks while Windows renegotiates the share.
  ///
  /// When either parks, the UI isolate parks with it: no frames, no SSE reads,
  /// no tool dispatch, so EVERY device tool call — including ones that spawn
  /// nothing — times out server-side with "Client did not respond within 90s".
  /// That is the "the client froze" report, and it is why the app has to be
  /// killed by hand. #178 bounded the shell and filesystem tools; the MCP
  /// process path was the remaining unbounded spawn on the UI isolate.
  ///
  /// Both stages are bounded now, and a wedged server degrades to "this MCP
  /// server did not start" through `outputStream` instead of taking the app.
  static const Duration spawnTimeout = Duration(seconds: 10);
  static const Duration resolveTimeout = Duration(seconds: 8);

  /// Bound on the JSON-RPC round trip. Kept below the server's client-tool
  /// stall watchdog so the model gets this error, not a stall.
  static const Duration requestTimeout = Duration(seconds: 30);

  ProcessManager({
    required this.command,
    required this.name,
    required this.args,
  });

  /// Starts the process with the configured command and arguments
  Future<bool> start() async {
    try {
      // For Windows, resolve the full path to the executable
      String executablePath = command;

      if (!kIsWeb && Platform.isWindows) {
        executablePath = await _resolveWindowsPath(command);
        if (executablePath.isEmpty) {
          _processOutput.add(
            "Error: Could not find executable '$command' in PATH",
          );
          print("Error: Could not find executable '$command' in PATH");
          return false;
        }
      }

      print("Starting process: $executablePath ${args.join(' ')}");

      _process = await Process.start(
        executablePath,
        args,
        mode: ProcessStartMode.normal,
      ).timeout(spawnTimeout);

      _isRunning = true;

      // Handle stdout
      _process!.stdout
          .transform(utf8.decoder)
          .transform(const LineSplitter())
          .listen((line) {
            if (!_isClosed) _handleProcessOutput(line);
          });

      // Handle stderr
      _process!.stderr
          .transform(utf8.decoder)
          .transform(const LineSplitter())
          .listen((line) {
            if (_isClosed) return;
            print("ERROR: [$name] $line");
            _processOutput.add("ERROR: $line");
          });

      // Handle process exit
      _process!.exitCode.then((exitCode) {
        _isRunning = false;
        print("Process $name exited with code $exitCode");
        if (!_isClosed) {
          _processOutput.add("Process exited with code $exitCode");
        }

        // Complete any pending requests with an error
        for (final entry in _responseCompleters.entries) {
          if (!entry.value.isCompleted) {
            entry.value.completeError("Process terminated");
          }
        }
        _responseCompleters.clear();
      });

      return true;
    } on TimeoutException {
      _processOutput.add(
        "Error: starting '$command' timed out after ${spawnTimeout.inSeconds}s "
        "(endpoint security, group policy, or a binary on a sleeping network "
        "share). This MCP server was not started.",
      );
      print("Starting '$command' timed out after ${spawnTimeout.inSeconds}s");
      return false;
    } catch (e) {
      print("Failed to start process $name: $e");
      _processOutput.add("Failed to start process: $e");
      return false;
    }
  }

  /// Sends a JSON-RPC request to the process and waits for the response
  Future<dynamic> sendRequest(String method, [dynamic params]) async {
    if (!_isRunning || _process == null) {
      throw Exception("Process is not running");
    }

    final id = _uuid.v4();
    final request = {
      'jsonrpc': '2.0',
      'id': id,
      'method': method,
      if (params != null) 'params': params,
    };

    final completer = Completer<dynamic>();
    _responseCompleters[id] = completer;

    try {
      final requestJson = jsonEncode(request);
      _process!.stdin.writeln(requestJson);
      await _process!.stdin.flush();

      // Set a timeout for the request
      return await completer.future.timeout(
        requestTimeout,
        onTimeout: () {
          _responseCompleters.remove(id);
          throw TimeoutException('Request timed out: $method');
        },
      );
    } catch (e) {
      _responseCompleters.remove(id);
      rethrow;
    }
  }

  /// Handles each line of output from the process
  void _handleProcessOutput(String line) {
    if (_isClosed) return;
    _processOutput.add(line);

    try {
      final json = jsonDecode(line);

      if (json is Map &&
          json.containsKey('id') &&
          json.containsKey('jsonrpc')) {
        final id = json['id'];
        if (_responseCompleters.containsKey(id)) {
          final completer = _responseCompleters.remove(id)!;

          if (json.containsKey('error')) {
            completer.completeError(json['error']);
          } else if (json.containsKey('result')) {
            completer.complete(json['result']);
          } else {
            completer.completeError('Invalid JSON-RPC response');
          }
        }
      }
    } catch (e) {
      // Not a JSON-RPC response, just regular output
    }
  }

  /// Resolves the full path to a Windows executable.
  ///
  /// Bounded via `runCommandBounded` (see shell_run.dart): `where.exe` can be
  /// held by endpoint security, and an unbounded `Process.run` here used to
  /// park the UI isolate inside `initMCP()`, freezing the app at startup.
  Future<String> _resolveWindowsPath(String cmd) async {
    try {
      final result = await runCommandBounded(
        'where',
        [cmd],
        timeout: resolveTimeout,
      );
      if (result.timedOut || result.spawnFailed) {
        debugPrint(
          'Path lookup for "$cmd" did not finish within ${resolveTimeout.inSeconds}s',
        );
        // Best effort: hand back the bare name and let the bounded spawn decide.
        return cmd;
      }
      if (result.exitCode == 0) {
        final paths = result.stdout.trim().split('\n');
        if (paths.isNotEmpty) {
          // Find the longest path
          return paths.reduce(
            (a, b) => a.trim().length > b.trim().length ? a.trim() : b.trim(),
          );
        }
      }

      return cmd;
    } catch (e) {
      debugPrint('Error resolving Windows path: $e');
      return '';
    }
  }

  /// Stops the process.
  ///
  /// `kill()` is SIGTERM; a child that traps it outlives the shutdown, and
  /// `stop()` is awaited on the tool-dispatch path (`stopConversation`), so a
  /// graceful-ignoring MCP server would park the UI isolate. Escalate to
  /// SIGKILL after a bounded grace, and never await the exit code on Windows
  /// where the kill is not observable.
  Future<void> stop() async {
    _isRunning = false;
    _isClosed = true;

    final proc = _process;
    if (proc != null) {
      try {
        proc.kill();
        if (!Platform.isWindows) {
          final exited = await proc.exitCode
              .timeout(const Duration(seconds: 3), onTimeout: () => -1);
          if (exited == -1) {
            proc.kill(ProcessSignal.sigkill);
          }
        }
      } catch (e) {
        debugPrint('Error killing process $name: $e');
      }
    }

    if (!_processOutput.isClosed) {
      await _processOutput.close();
    }
  }
}
