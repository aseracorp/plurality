import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/foundation.dart';
import 'package:path/path.dart' as path;
import 'package:uuid/uuid.dart';

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

      if (Platform.isWindows) {
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
      );

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
        const Duration(seconds: 30),
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

  /// Resolves the full path to a Windows executable
  Future<String> _resolveWindowsPath(String cmd) async {
    try {
      final result = await Process.run('where', [cmd]);
      if (result.exitCode == 0) {
        final paths = (result.stdout as String).trim().split('\n');
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

  /// Stops the process
  Future<void> stop() async {
    _isRunning = false;
    _isClosed = true;

    if (_process != null) {
      try {
        _process!.kill();
      } catch (e) {
        debugPrint('Error killing process: $e');
      }
    }

    await _processOutput.close();
  }
}
