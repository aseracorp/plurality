import 'dart:async';
import 'dart:io';

import 'package:flutter/foundation.dart' show compute, kIsWeb;
import 'package:path/path.dart' as path;

import 'filesystem_ops.dart';

/// Singleton that exposes two namespaced device-side filesystem tools to the
/// LLM and executes them locally, sandboxed to a folder the user has attached
/// to the conversation. Path inputs from the LLM are interpreted as RELATIVE
/// to the sandbox root; '..' and absolute paths are rejected; the resolved
/// path is verified to stay inside the sandbox.
///
/// Every filesystem syscall runs on a worker isolate (`compute`) and is
/// time-boxed. `listSync`, `readAsBytesSync`, `writeAsStringSync` and
/// `existsSync` park the calling isolate for the whole duration of the call,
/// and on Windows the attached folder is routinely a network share
/// (Tailscale / SMB / OneDrive) whose peer can be asleep — a single such call
/// then blocks indefinitely.
///
/// `executeFsRead` / `executeFsWrite` are dispatched from ChatService's tool
/// loop, which runs on the UI isolate. Blocking there freezes the whole app:
/// no frames, no SSE reads, no tool dispatch. The server's client-tool
/// watchdog synthesizes "Client did not respond within 90s" and resumes the
/// LLM loop, while the blocked isolate is never reaped — so every later device
/// call, the device shell included, times out as well. To the user the client
/// is "crashed"; to the model every device tool becomes unusable.
///
/// A device tool call is a capability, not a UI operation, so its work belongs
/// off the UI isolate and under a bound that yields an honest, actionable
/// error instead of a stall. The bound is deliberately shorter than the
/// server's `clientToolStallTimeout` (server/src/ai/tool_loop.go) so the model
/// receives this error and can fall back to a server-side tool, rather than
/// the loop stalling on the watchdog.
///
/// The syscall bodies live in `filesystem_ops.dart` at top level: a top-level
/// function is what `compute()` can send, and keeping them there also makes
/// them directly unit-testable.
class FilesystemService {
  static const String readToolName = 'filesystem_client__fs_read';
  static const String writeToolName = 'filesystem_client__fs_write';

  /// Bound on the sandbox-root existence probe. Short on purpose: a cold
  /// network share should answer "not reachable" quickly, not stall the turn
  /// while Windows renegotiates the connection.
  static const Duration rootProbeTimeout = Duration(seconds: 5);

  /// Bound on the filesystem operation itself.
  static const Duration opTimeout = Duration(seconds: 25);

  static final FilesystemService _instance = FilesystemService._internal();
  factory FilesystemService() => _instance;
  FilesystemService._internal();

  /// Tool definitions to advertise to the LLM via `clientSideTools`. The
  /// shape mirrors `FsClientReadToolRequest` / `FsClientWriteToolRequest` on
  /// the server (server/src/ai_tools/filesystem_client.go).
  List<Map<String, dynamic>> getToolDefinitions() {
    return [
      {
        'name': readToolName,
        'description': "[device] Read files in the user's attached folder. Set 'op' to one of: list (directory entries), find (recursive name pattern match), read (whole file as text), read_segment (line range), stat (metadata). Paths are RELATIVE to the attached folder root; '..' is rejected.",
        'parameters': {
          'type': 'object',
          'properties': {
            'op': {
              'type': 'string',
              'description':
                  'Operation: list | find | read | read_segment | stat',
              'enum': ['list', 'find', 'read', 'read_segment', 'stat'],
            },
            'path': {
              'type': 'string',
              'description':
                  "Path RELATIVE to the attached folder. Use '.' for the root.",
            },
            'pattern': {
              'type': 'string',
              'description': "For 'find': glob pattern matched against entry names (e.g. '*.dart').",
            },
            'recursive': {
              'type': 'string',
              'description': "For 'list': 'true' to recurse, 'false' for shallow listing (default: false).",
            },
            'start_line': {
              'type': 'integer',
              'description':
                  "For 'read_segment': 1-based starting line (inclusive).",
            },
            'end_line': {
              'type': 'integer',
              'description': "For 'read_segment': 1-based ending line (inclusive). 0 or omitted means to end of file.",
            },
          },
          'required': ['op', 'path'],
        },
      },
      {
        'name': writeToolName,
        'description': "[device] Modify files in the user's attached folder. Set 'op' to one of: create (write a new file, fails if exists), edit (single occurrence search-and-replace), copy, move, delete, mkdir. Paths are RELATIVE to the attached folder root; '..' is rejected.",
        'parameters': {
          'type': 'object',
          'properties': {
            'op': {
              'type': 'string',
              'description':
                  'Operation: create | edit | copy | move | delete | mkdir',
              'enum': ['create', 'edit', 'copy', 'move', 'delete', 'mkdir'],
            },
            'path': {
              'type': 'string',
              'description': 'Target path RELATIVE to the attached folder.',
            },
            'dest_path': {
              'type': 'string',
              'description': "For 'copy' and 'move': destination path RELATIVE to the attached folder.",
            },
            'content': {
              'type': 'string',
              'description': "For 'create': the file's text content.",
            },
            'old_text': {
              'type': 'string',
              'description': "For 'edit': literal substring to find. Must occur exactly once in the file.",
            },
            'new_text': {
              'type': 'string',
              'description': "For 'edit': replacement text.",
            },
          },
          'required': ['op', 'path'],
        },
      },
    ];
  }

  /// Resolve the sandbox root and prove it exists, without blocking the UI
  /// isolate. The existence probe is an async `Directory.exists()` and is
  /// time-boxed — see the class doc.
  Future<String?> _resolveRoot(String? sandboxRoot) async {
    final normalized = validateRootPath(sandboxRoot);
    if (normalized == null) return null;
    if (kIsWeb) return normalized;
    try {
      final ok = await Directory(normalized)
          .exists()
          .timeout(rootProbeTimeout, onTimeout: () => false);
      return ok ? normalized : null;
    } catch (_) {
      return null;
    }
  }

  /// Root is syntactically valid but not reachable right now. Phrased so the
  /// model stops retrying a path this device cannot see.
  static String _unreachableRoot(String sandboxRoot) =>
      'Error: the attached folder "$sandboxRoot" is not reachable from this device '
      '(missing, permission denied, or an offline network share). '
      'Ask the user to make it available, then retry once — do not retry it repeatedly.';

  /// Run [req] on a worker isolate, bounded by [opTimeout]. The bound is the
  /// point: an unbounded sync syscall against a sleeping share stalls the UI
  /// isolate forever, which the server reports as "Client did not respond
  /// within 90s" for this call and for every device call after it.
  Future<String> _runBounded(FsRequest req) async {
    try {
      return await compute(runFsOp, req).timeout(opTimeout);
    } on TimeoutException {
      final target = req.path.isEmpty ? req.root : req.path;
      return 'Error: "${req.op}" on "$target" timed out after ${opTimeout.inSeconds}s. '
          'The device filesystem is not responding (a sleeping Tailscale/SMB/OneDrive '
          'peer does exactly this). Do not retry this path — use a server-side tool, '
          'or ask the user to wake the share / attach a local folder.';
    } catch (e) {
      return 'Error: ${req.op} failed: $e';
    }
  }

  Future<String> executeFsRead(
    String? sandboxRoot,
    Map<String, dynamic> args,
  ) async {
    final root = await _resolveRoot(sandboxRoot);
    if (root == null) {
      return sandboxRoot == null || sandboxRoot.isEmpty
          ? 'Error: no folder is attached to this conversation. Ask the user to attach one.'
          : _unreachableRoot(sandboxRoot);
    }

    final op = (args['op'] as String?) ?? '';
    final relPath = (args['path'] as String?) ?? '';
    if (op.isEmpty) return "Error: 'op' is required.";
    if (relPath.isEmpty) return "Error: 'path' is required.";

    final resolved = _resolveInside(root, relPath);
    if (resolved == null) {
      return 'Error: path is outside the attached folder.';
    }

    const readOps = {'list', 'find', 'read', 'read_segment', 'stat'};
    if (!readOps.contains(op)) return 'Error: unknown op "$op".';
    if (op == 'find' && (args['pattern'] as String?)?.isEmpty != false) {
      return "Error: 'pattern' is required for find.";
    }

    return _runBounded(
      FsRequest(
        op: op,
        root: root,
        path: resolved,
        pattern: (args['pattern'] as String?) ?? '',
        recursive: args['recursive']?.toString().toLowerCase() == 'true',
        startLine: _asInt(args['start_line']),
        endLine: _asInt(args['end_line']),
      ),
    );
  }

  Future<String> executeFsWrite(
    String? sandboxRoot,
    Map<String, dynamic> args,
  ) async {
    final root = await _resolveRoot(sandboxRoot);
    if (root == null) {
      return sandboxRoot == null || sandboxRoot.isEmpty
          ? 'Error: no folder is attached to this conversation. Ask the user to attach one.'
          : _unreachableRoot(sandboxRoot);
    }

    final op = (args['op'] as String?) ?? '';
    final relPath = (args['path'] as String?) ?? '';
    if (op.isEmpty) return "Error: 'op' is required.";
    if (relPath.isEmpty) return "Error: 'path' is required.";

    final resolved = _resolveInside(root, relPath);
    if (resolved == null) {
      return 'Error: path is outside the attached folder.';
    }

    var destResolved = '';
    if (op == 'copy' || op == 'move') {
      final destRel = (args['dest_path'] as String?) ?? '';
      if (destRel.isEmpty) return "Error: 'dest_path' is required for $op.";
      final d = _resolveInside(root, destRel);
      if (d == null) {
        return 'Error: dest_path is outside the attached folder.';
      }
      destResolved = d;
    }
    if (op == 'edit' && ((args['old_text'] as String?) ?? '').isEmpty) {
      return "Error: 'old_text' is required for edit.";
    }

    const writeOps = {'create', 'edit', 'copy', 'move', 'delete', 'mkdir'};
    if (!writeOps.contains(op)) return 'Error: unknown op "$op".';

    return _runBounded(
      FsRequest(
        op: op,
        root: root,
        path: resolved,
        destPath: destResolved,
        content: (args['content'] as String?) ?? '',
        oldText: (args['old_text'] as String?) ?? '',
        newText: (args['new_text'] as String?) ?? '',
      ),
    );
  }

  // --- path safety ---

  /// Pure, I/O-free sandbox-root validation. Split out from reachability so it
  /// is unit-testable, and so the only blocking step left on the tool path is
  /// the single time-boxed `exists` probe in [_resolveRoot].
  static String? validateRootPath(String? root) {
    if (root == null || root.isEmpty) return null;
    // Refuse to accept a relative sandbox root — `path.canonicalize` would
    // silently fall back to the current working directory (on Windows
    // desktop that's typically C:\Users\<user>), leaking the user's home
    // dir as the sandbox.
    if (!path.isAbsolute(root)) return null;
    return path.normalize(root);
  }

  /// Returns the absolute path if [relPath] resolves inside [root], otherwise
  /// null. Rejects '..' segments and absolute paths. Uses [path.normalize]
  /// rather than [path.canonicalize] so the resolver never falls back to the
  /// current working directory.
  static String? resolveInside(String root, String relPath) {
    // Treat '.' (and a couple of trivially-equivalent forms) as the root
    // itself — no join/normalize round-trip needed.
    if (relPath.isEmpty ||
        relPath == '.' ||
        relPath == './' ||
        relPath == r'.\') {
      return root;
    }
    if (relPath.contains('..')) return null;
    if (path.isAbsolute(relPath)) return null;
    final joined = path.join(root, relPath);
    final resolved = path.normalize(joined);
    if (!path.equals(resolved, root) && !path.isWithin(root, resolved)) {
      return null;
    }
    return resolved;
  }

  /// Instance alias kept for call-site stability.
  String? _resolveInside(String root, String relPath) =>
      resolveInside(root, relPath);

  int _asInt(dynamic v) {
    if (v == null) return 0;
    if (v is int) return v;
    if (v is double) return v.toInt();
    if (v is String) return int.tryParse(v) ?? 0;
    return 0;
  }
}
