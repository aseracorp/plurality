import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import 'dart:typed_data';

/// Dart port of server/src/ai_tools/backgroundProcess.go. Tracks shell
/// commands the AI started in 'start' mode so subsequent 'status'/'kill'/'list'
/// calls can refer to them by task_id. Tasks live in memory only — a client
/// restart loses them, matching the server semantics.

const int kBgStdoutLimit = 1024 * 1024; // 1 MiB
const int kBgStderrLimit = 256 * 1024;  // 256 KiB
const Duration kBgRetention = Duration(hours: 1);
const Duration kBgGCInterval = Duration(minutes: 15);

/// Fixed-size circular byte buffer. Older bytes are dropped when capacity is
/// exceeded so the buffer always holds the most recent [limit] bytes.
class CapBuffer {
  final int limit;
  final BytesBuilder _builder = BytesBuilder(copy: false);

  CapBuffer(this.limit);

  void write(List<int> chunk) {
    _builder.add(chunk);
    if (_builder.length > limit) {
      final all = _builder.takeBytes();
      final start = all.length - limit;
      _builder.add(all.sublist(start));
    }
  }

  String tail(int n) {
    final bytes = _builder.toBytes();
    if (n <= 0 || n >= bytes.length) return _decode(bytes);
    return _decode(bytes.sublist(bytes.length - n));
  }

  int get length => _builder.length;

  static String _decode(Uint8List bytes) {
    // allowMalformed: a trailing partial UTF-8 sequence at the buffer
    // boundary shouldn't blow up — the bad bytes get replaced with U+FFFD.
    return utf8.decode(bytes, allowMalformed: true);
  }
}

class BgProcess {
  final String taskId;
  final String command;
  final String pwd;
  final Process process;
  final CapBuffer stdout;
  final CapBuffer stderr;
  final DateTime startedAt;
  DateTime? endedAt;
  String state; // "running" | "exited" | "killed" | "error"
  int exitCode; // -1 while running
  String errMsg;
  final Completer<void> done;

  BgProcess({
    required this.taskId,
    required this.command,
    required this.pwd,
    required this.process,
    required this.stdout,
    required this.stderr,
  })  : startedAt = DateTime.now(),
        state = 'running',
        exitCode = -1,
        errMsg = '',
        done = Completer<void>();

  int get pid => process.pid;

  Duration get duration {
    final end = endedAt ?? DateTime.now();
    return end.difference(startedAt);
  }
}

class BgProcessRegistry {
  static final BgProcessRegistry _instance = BgProcessRegistry._internal();
  factory BgProcessRegistry() => _instance;
  BgProcessRegistry._internal();

  final Map<String, BgProcess> _tasks = {};
  final Random _rng = Random.secure();
  Timer? _gcTimer;

  String _newTaskID() {
    final bytes = List<int>.generate(4, (_) => _rng.nextInt(256));
    return bytes.map((b) => b.toRadixString(16).padLeft(2, '0')).join();
  }

  /// Spawn [process] (already started) and register it under a fresh task_id.
  /// Stdout/stderr are piped into capped buffers. The wait future updates
  /// terminal state when the process exits.
  Future<BgProcess> register({
    required String command,
    required String pwd,
    required Process process,
  }) async {
    final stdoutBuf = CapBuffer(kBgStdoutLimit);
    final stderrBuf = CapBuffer(kBgStderrLimit);
    process.stdout.listen(stdoutBuf.write);
    process.stderr.listen(stderrBuf.write);

    final bp = BgProcess(
      taskId: _newTaskID(),
      command: command,
      pwd: pwd,
      process: process,
      stdout: stdoutBuf,
      stderr: stderrBuf,
    );
    _tasks[bp.taskId] = bp;

    process.exitCode.then((code) {
      bp.endedAt = DateTime.now();
      // If state is already "killed" preserve it — a user-initiated kill wins
      // regardless of how the process actually terminated.
      if (bp.state != 'killed') {
        bp.exitCode = code;
        bp.state = code == 0 ? 'exited' : 'exited';
      } else if (code != -1) {
        bp.exitCode = code;
      }
      if (!bp.done.isCompleted) bp.done.complete();
    }).catchError((e) {
      bp.endedAt = DateTime.now();
      if (bp.state != 'killed') {
        bp.state = 'error';
        bp.errMsg = e.toString();
      }
      if (!bp.done.isCompleted) bp.done.complete();
    });

    _startGCOnce();
    return bp;
  }

  BgProcess? get(String taskId) => _tasks[taskId];

  List<BgProcess> list() {
    final out = _tasks.values.toList();
    out.sort((a, b) => b.startedAt.compareTo(a.startedAt));
    return out;
  }

  /// Mark as killed and signal the process. Waits up to [settleTimeout] for
  /// the exit goroutine to settle exit code.
  Future<bool> kill(String taskId, Duration settleTimeout) async {
    final bp = _tasks[taskId];
    if (bp == null) return false;
    final alreadyDone = bp.state != 'running';
    if (!alreadyDone) {
      bp.state = 'killed';
      try {
        bp.process.kill(ProcessSignal.sigterm);
      } catch (_) {}
      await bp.done.future.timeout(settleTimeout, onTimeout: () {});
    }
    return true;
  }

  void _startGCOnce() {
    if (_gcTimer != null) return;
    _gcTimer = Timer.periodic(kBgGCInterval, (_) {
      final cutoff = DateTime.now().subtract(kBgRetention);
      _tasks.removeWhere((_, bp) {
        final end = bp.endedAt;
        return end != null && end.isBefore(cutoff);
      });
    });
  }
}
