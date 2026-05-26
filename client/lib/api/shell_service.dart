import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:path/path.dart' as path;

import 'shell_background_process.dart';

/// Singleton that exposes a namespaced device-side shell tool to the LLM and
/// executes it locally via PowerShell (Windows) or sh (macOS/Linux). Mirrors
/// the contract of server/src/ai_tools/shellExec.go so the LLM sees identical
/// output regardless of whether a call ran on server or client.
///
/// The tool description includes a runtime environment block (OS, default
/// shell, attached folder + git branch, platform syntax gotchas) so the model
/// writes correct commands without exploratory probing.
class ShellService {
  static const String toolName = 'shell_client__exec';

  static const int _foregroundTimeoutSeconds = 60;
  static const int _defaultStatusTailBytes = 4096;
  static const int _stdoutResponseCap = 50000;
  static const int _stderrResponseCap = 10000;

  static final ShellService _instance = ShellService._internal();
  factory ShellService() => _instance;
  ShellService._internal();

  // Cached at first call — re-derivable, but cheap to memoize.
  String? _cachedShellInfo;

  /// Returns the tool definition to advertise to the LLM via `clientSideTools`.
  /// The shape mirrors `ShellClientExecToolRequest` on the server
  /// (server/src/ai_tools/shell_client.go); the description is enriched with
  /// the current environment block.
  Map<String, dynamic> getToolDefinition({String? attachedFolder}) {
    return {
      'name': toolName,
      'description': _buildDescription(attachedFolder),
      'parameters': {
        'type': 'object',
        'properties': {
          'operation': {
            'type': 'string',
            'description':
                "What to do. 'exec' (default) runs the command and waits up to ${_foregroundTimeoutSeconds}s. 'start' spawns a background task and returns a task_id immediately. 'status' returns state + tail of output for a task_id. 'kill' terminates a task_id. 'list' returns all known tasks.",
            'enum': ['exec', 'start', 'status', 'kill', 'list'],
          },
          'command': {
            'type': 'string',
            'description':
                "The shell command to execute. Required for 'exec' and 'start'.",
          },
          'pwd': {
            'type': 'string',
            'description':
                "Working directory. Absolute paths used as-is; relative paths resolve against the attached folder if any, else the user's home directory.",
          },
          'task_id': {
            'type': 'string',
            'description':
                "Task identifier returned by 'start'. Required for 'status' and 'kill'.",
          },
          'tail_bytes': {
            'type': 'integer',
            'description':
                "How many bytes of the most recent stdout/stderr to return on 'status' (default $_defaultStatusTailBytes). Older bytes beyond ~1 MiB stdout / 256 KiB stderr are not retained.",
          },
        },
      },
    };
  }

  // --- description / env block ---

  String _buildDescription(String? attachedFolder) {
    final env = StringBuffer();
    env.writeln(
      "[device] Execute a shell command on the USER'S DEVICE and return its output. Operation 'exec' (default) runs synchronously with a ${_foregroundTimeoutSeconds}s timeout. For long-running processes use 'start' to spawn a background task, then 'status' to poll, 'kill' to terminate, and 'list' to see all tasks. Background tasks live in client memory only — a client restart loses them.",
    );
    env.writeln();
    env.writeln('Device environment:');
    env.writeln('  OS: ${_osLine()}');
    env.writeln('  Default shell: ${_shellLine()}');
    env.writeln('  Home: ${_homeDir()}');
    env.writeln('  Default cwd: ${_defaultCwd(attachedFolder)}');
    final git = _gitInfo(attachedFolder);
    if (git.isNotEmpty) {
      env.writeln('  Attached folder git:');
      for (final line in git) {
        env.writeln('    $line');
      }
    }
    env.writeln();
    env.writeln('Platform notes:');
    for (final note in _gotchas()) {
      env.writeln('  - $note');
    }
    return env.toString();
  }

  String _osLine() {
    if (kIsWeb) return 'web';
    final os = Platform.operatingSystem;
    final ver = Platform.operatingSystemVersion;
    return '$os ($ver)';
  }

  String _shellLine() {
    if (_cachedShellInfo != null) return _cachedShellInfo!;
    if (kIsWeb) {
      _cachedShellInfo = 'n/a';
    } else if (Platform.isWindows) {
      _cachedShellInfo = _probePowerShellVersion();
    } else {
      final envShell = Platform.environment['SHELL'] ?? '/bin/sh';
      _cachedShellInfo = envShell;
    }
    return _cachedShellInfo!;
  }

  String _probePowerShellVersion() {
    try {
      final r = Process.runSync(
        'powershell.exe',
        ['-NoProfile', '-Command', r'$PSVersionTable.PSVersion.ToString()'],
      );
      if (r.exitCode == 0) {
        final v = (r.stdout as String).trim();
        if (v.isNotEmpty) return 'powershell.exe ($v)';
      }
    } catch (_) {}
    return 'powershell.exe';
  }

  String _homeDir() {
    if (kIsWeb) return '';
    if (Platform.isWindows) {
      return Platform.environment['USERPROFILE'] ?? '';
    }
    return Platform.environment['HOME'] ?? '';
  }

  String _defaultCwd(String? attachedFolder) {
    if (attachedFolder != null && attachedFolder.isNotEmpty) {
      return attachedFolder;
    }
    return _homeDir();
  }

  List<String> _gitInfo(String? attachedFolder) {
    if (attachedFolder == null || attachedFolder.isEmpty) return const [];
    try {
      final check = Process.runSync(
        'git',
        ['-C', attachedFolder, 'rev-parse', '--is-inside-work-tree'],
      );
      if (check.exitCode != 0) return const [];
      final out = <String>[];
      final branch = Process.runSync(
        'git',
        ['-C', attachedFolder, 'branch', '--show-current'],
      );
      if (branch.exitCode == 0) {
        final b = (branch.stdout as String).trim();
        if (b.isNotEmpty) out.add('Current branch: $b');
      }
      final log = Process.runSync(
        'git',
        ['-C', attachedFolder, 'log', '--oneline', '-3'],
      );
      if (log.exitCode == 0) {
        final lines = (log.stdout as String).trim().split('\n');
        if (lines.isNotEmpty && lines.first.isNotEmpty) {
          out.add('Recent commits:');
          for (final l in lines) {
            out.add('  $l');
          }
        }
      }
      return out;
    } catch (_) {
      return const [];
    }
  }

  List<String> _gotchas() {
    if (!kIsWeb && Platform.isWindows) {
      return const [
        'PowerShell 5.1: pipeline operators `&&` / `||` are NOT available. To run B only if A succeeds: `A; if (\$?) { B }`. To chain unconditionally: `A; B`.',
        'Ternary `?:`, null-coalescing `??`, and null-conditional `?.` operators are NOT available in PS 5.1.',
        'Avoid `2>&1` on native exes: it wraps each stderr line in an ErrorRecord and sets `\$?` to `\$false` even on exit 0.',
        'Default file encoding is UTF-16 LE BOM. Pass `-Encoding utf8` to Out-File/Set-Content for tools to read it correctly.',
        'Env vars: `\$env:NAME` (read/write), NOT `\$NAME` or bash `export`.',
        'Registry paths use PSDrive prefixes (`HKLM:\\SOFTWARE\\...`), not raw `HKEY_LOCAL_MACHINE\\...`.',
        'Bash control-flow (`if [ -f x ]`, `for x in *`, backticks) is a parser error — use PowerShell equivalents.',
      ];
    }
    if (!kIsWeb && Platform.isMacOS) {
      return const [
        'macOS ships BSD utilities by default: `sed -i` requires an empty backup arg (`sed -i ""`); GNU-only flags differ.',
        '`readlink -f` is not in BSD `readlink` — use `python3 -c "import os,sys; print(os.path.realpath(sys.argv[1]))" <path>` or install coreutils.',
        'Prefer `gtimeout`/`gdate` (from coreutils) over `timeout`/`date -d` which are GNU-only.',
      ];
    }
    return const [
      'GNU coreutils is typically available.',
      '/bin/sh is the executor — use POSIX syntax. For bash-only features prefix with `bash -c`.',
    ];
  }

  // --- dispatch ---

  Future<String> executeShellExec(
    String? sandboxRoot,
    Map<String, dynamic> args,
  ) async {
    final op = (args['operation'] as String?)?.trim().isEmpty ?? true
        ? 'exec'
        : (args['operation'] as String).trim();
    switch (op) {
      case 'exec':
        return _runForeground(args, sandboxRoot);
      case 'start':
        return _runStart(args, sandboxRoot);
      case 'status':
        return _runStatus(args);
      case 'kill':
        return _runKill(args);
      case 'list':
        return _runList();
      default:
        return 'Error: unknown operation "$op". Use one of: exec, start, status, kill, list.';
    }
  }

  // --- exec (foreground) ---

  Future<String> _runForeground(
    Map<String, dynamic> args,
    String? sandboxRoot,
  ) async {
    final command = (args['command'] as String?) ?? '';
    if (command.isEmpty) {
      return "Error: 'command' parameter is required for operation 'exec'.";
    }
    final pwd = _resolvePwd(args['pwd'] as String?, sandboxRoot);

    final shell = _shellCommand(command);
    final started = DateTime.now();
    Process proc;
    try {
      proc = await Process.start(
        shell.executable,
        shell.args,
        workingDirectory: pwd.isEmpty ? null : pwd,
        runInShell: false,
      );
    } catch (e) {
      return 'Error starting command: $e';
    }

    final stdoutFuture = proc.stdout.transform(utf8.decoder).join();
    final stderrFuture = proc.stderr.transform(utf8.decoder).join();

    var timedOut = false;
    final timer = Timer(Duration(seconds: _foregroundTimeoutSeconds), () {
      timedOut = true;
      try {
        proc.kill(ProcessSignal.sigterm);
      } catch (_) {}
    });

    final exitCode = await proc.exitCode;
    timer.cancel();
    final stdoutStr = await stdoutFuture;
    final stderrStr = await stderrFuture;
    final duration = DateTime.now().difference(started);

    final out = StringBuffer();
    out.writeln('Command: $command');
    if (pwd.isNotEmpty) out.writeln('Working directory: $pwd');
    out.writeln('Duration: ${_formatMs(duration)}');
    if (timedOut) {
      out.writeln(
        'Status: TIMED OUT after $_foregroundTimeoutSeconds seconds (process killed, partial output below). For long-running processes use operation \'start\' instead.',
      );
    } else {
      out.writeln('Exit code: $exitCode');
    }
    out.writeln();
    out.writeln('--- STDOUT ---');
    out.writeln(_capForResponse(stdoutStr, _stdoutResponseCap));
    out.writeln();
    out.writeln('--- STDERR ---');
    out.writeln(_capForResponse(stderrStr, _stderrResponseCap));
    return out.toString();
  }

  // --- background ops ---

  Future<String> _runStart(
    Map<String, dynamic> args,
    String? sandboxRoot,
  ) async {
    final command = (args['command'] as String?) ?? '';
    if (command.isEmpty) {
      return "Error: 'command' parameter is required for operation 'start'.";
    }
    final pwd = _resolvePwd(args['pwd'] as String?, sandboxRoot);
    final shell = _shellCommand(command);
    Process proc;
    try {
      proc = await Process.start(
        shell.executable,
        shell.args,
        workingDirectory: pwd.isEmpty ? null : pwd,
        runInShell: false,
        mode: ProcessStartMode.normal,
      );
    } catch (e) {
      return 'Error starting background task: $e';
    }
    final bp = await BgProcessRegistry().register(
      command: command,
      pwd: pwd,
      process: proc,
    );
    final out = StringBuffer();
    out.writeln('Background task started.');
    out.writeln('task_id: ${bp.taskId}');
    out.writeln('pid: ${bp.pid}');
    out.writeln('command: ${bp.command}');
    if (bp.pwd.isNotEmpty) out.writeln('working directory: ${bp.pwd}');
    out.writeln('started_at: ${bp.startedAt.toIso8601String()}');
    out.writeln();
    out.write(
      "Use operation 'status' with this task_id to read output, or 'kill' to terminate it.",
    );
    return out.toString();
  }

  String _runStatus(Map<String, dynamic> args) {
    final taskId = (args['task_id'] as String?) ?? '';
    if (taskId.isEmpty) {
      return "Error: 'task_id' parameter is required for operation 'status'.";
    }
    final bp = BgProcessRegistry().get(taskId);
    if (bp == null) {
      return 'Error: no background task with task_id "$taskId". It may have been garbage-collected (tasks are retained for 1 hour after completion) or the client was restarted.';
    }
    final tail = _asInt(args['tail_bytes'], _defaultStatusTailBytes);
    return _formatStatus(bp, tail <= 0 ? _defaultStatusTailBytes : tail);
  }

  Future<String> _runKill(Map<String, dynamic> args) async {
    final taskId = (args['task_id'] as String?) ?? '';
    if (taskId.isEmpty) {
      return "Error: 'task_id' parameter is required for operation 'kill'.";
    }
    final ok = await BgProcessRegistry()
        .kill(taskId, const Duration(seconds: 2));
    if (!ok) {
      return 'Error: no background task with task_id "$taskId".';
    }
    final bp = BgProcessRegistry().get(taskId);
    if (bp == null) {
      return 'task_id: $taskId\nstate: killed\n';
    }
    return 'Kill requested.\n\n${_formatStatus(bp, _defaultStatusTailBytes)}';
  }

  String _runList() {
    final tasks = BgProcessRegistry().list();
    if (tasks.isEmpty) {
      return "No background tasks. Use operation 'start' to launch one.";
    }
    final out = StringBuffer();
    out.writeln('Known background tasks (${tasks.length}):');
    for (final t in tasks) {
      final exitStr = t.state == 'running' ? '' : ' exit=${t.exitCode}';
      out.writeln(
        '- ${t.taskId} [${t.state}$exitStr] (${_formatMs(t.duration)}) pid=${t.pid} cmd="${_truncate(t.command, 120)}"',
      );
    }
    out.write("\nUse 'status' with a task_id to read output.");
    return out.toString();
  }

  String _formatStatus(BgProcess bp, int tailBytes) {
    final stdoutTail = bp.stdout.tail(tailBytes);
    final stderrTail = bp.stderr.tail(tailBytes);
    final out = StringBuffer();
    out.writeln('task_id: ${bp.taskId}');
    out.writeln('command: ${bp.command}');
    if (bp.pwd.isNotEmpty) out.writeln('working directory: ${bp.pwd}');
    out.writeln('pid: ${bp.pid}');
    out.writeln('state: ${bp.state}');
    out.writeln('duration: ${_formatMs(bp.duration)}');
    if (bp.state != 'running') {
      out.writeln('exit code: ${bp.exitCode}');
      if (bp.errMsg.isNotEmpty) out.writeln('error: ${bp.errMsg}');
      if (bp.endedAt != null) {
        out.writeln('ended_at: ${bp.endedAt!.toIso8601String()}');
      }
    }
    out.writeln();
    out.writeln(
      '--- STDOUT (showing last ${stdoutTail.length} of ${bp.stdout.length} bytes) ---',
    );
    out.writeln(_capForResponse(stdoutTail, _stdoutResponseCap));
    out.writeln();
    out.writeln(
      '--- STDERR (showing last ${stderrTail.length} of ${bp.stderr.length} bytes) ---',
    );
    out.write(_capForResponse(stderrTail, _stderrResponseCap));
    return out.toString();
  }

  // --- helpers ---

  String _resolvePwd(String? raw, String? sandboxRoot) {
    final p = (raw ?? '').trim();
    if (p.isEmpty) {
      if (sandboxRoot != null && sandboxRoot.isNotEmpty) return sandboxRoot;
      return _homeDir();
    }
    if (path.isAbsolute(p)) return p;
    final base =
        (sandboxRoot != null && sandboxRoot.isNotEmpty) ? sandboxRoot : _homeDir();
    return path.normalize(path.join(base, p));
  }

  _ShellCommand _shellCommand(String command) {
    if (!kIsWeb && Platform.isWindows) {
      return _ShellCommand(
        executable: 'powershell.exe',
        args: ['-NoProfile', '-NonInteractive', '-Command', command],
      );
    }
    return _ShellCommand(executable: '/bin/sh', args: ['-c', command]);
  }

  String _capForResponse(String s, int cap) {
    if (s.isEmpty) return '(empty)';
    if (s.length <= cap) return s;
    return '${s.substring(0, cap)}\n... (truncated)';
  }

  String _formatMs(Duration d) {
    final ms = d.inMilliseconds;
    if (ms < 1000) return '${ms}ms';
    final s = (ms / 1000.0).toStringAsFixed(3);
    return '${s}s';
  }

  String _truncate(String s, int n) =>
      s.length <= n ? s : '${s.substring(0, n)}...';

  int _asInt(dynamic v, int fallback) {
    if (v == null) return fallback;
    if (v is int) return v;
    if (v is double) return v.toInt();
    if (v is String) return int.tryParse(v) ?? fallback;
    return fallback;
  }
}

class _ShellCommand {
  final String executable;
  final List<String> args;
  const _ShellCommand({required this.executable, required this.args});
}
