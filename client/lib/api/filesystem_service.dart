import 'dart:convert';
import 'dart:io';
import 'package:path/path.dart' as path;

/// Singleton that exposes two namespaced device-side filesystem tools to the
/// LLM and executes them locally, sandboxed to a folder the user has attached
/// to the conversation. Path inputs from the LLM are interpreted as RELATIVE
/// to the sandbox root; '..' and absolute paths are rejected; the canonical
/// resolved path is verified to stay inside the sandbox.
class FilesystemService {
  static const String readToolName = 'filesystem_client__fs_read';
  static const String writeToolName = 'filesystem_client__fs_write';

  static const int _maxReadBytes = 200 * 1024;
  static const int _maxListEntries = 500;
  static const int _maxFindResults = 500;

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
        'description':
            "[device] Read files in the user's attached folder. Set 'op' to one of: list (directory entries), find (recursive name pattern match), read (whole file as text), read_segment (line range), stat (metadata). Paths are RELATIVE to the attached folder root; '..' is rejected.",
        'parameters': {
          'type': 'object',
          'properties': {
            'op': {
              'type': 'string',
              'description': 'Operation: list | find | read | read_segment | stat',
              'enum': ['list', 'find', 'read', 'read_segment', 'stat'],
            },
            'path': {
              'type': 'string',
              'description': "Path RELATIVE to the attached folder. Use '.' for the root.",
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
              'description': "For 'read_segment': 1-based starting line (inclusive).",
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
        'description':
            "[device] Modify files in the user's attached folder. Set 'op' to one of: create (write a new file, fails if exists), edit (single occurrence search-and-replace), copy, move, delete, mkdir. Paths are RELATIVE to the attached folder root; '..' is rejected.",
        'parameters': {
          'type': 'object',
          'properties': {
            'op': {
              'type': 'string',
              'description': 'Operation: create | edit | copy | move | delete | mkdir',
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

  Future<String> executeFsRead(String? sandboxRoot, Map<String, dynamic> args) async {
    final root = _validateRoot(sandboxRoot);
    if (root == null) {
      return 'Error: no folder is attached to this conversation. Ask the user to attach one.';
    }

    final op = (args['op'] as String?) ?? '';
    final relPath = (args['path'] as String?) ?? '';
    if (op.isEmpty) return "Error: 'op' is required.";
    if (relPath.isEmpty) return "Error: 'path' is required.";

    final resolved = _resolveInside(root, relPath);
    if (resolved == null) {
      return 'Error: path is outside the attached folder.';
    }

    switch (op) {
      case 'list':
        final recursive =
            (args['recursive']?.toString().toLowerCase() == 'true');
        return _list(root, resolved, recursive);
      case 'find':
        final pattern = (args['pattern'] as String?) ?? '';
        if (pattern.isEmpty) return "Error: 'pattern' is required for find.";
        return _find(root, resolved, pattern);
      case 'read':
        return _read(resolved);
      case 'read_segment':
        final start = _asInt(args['start_line']);
        final end = _asInt(args['end_line']);
        return _readSegment(resolved, start, end);
      case 'stat':
        return _stat(resolved);
      default:
        return 'Error: unknown op "$op".';
    }
  }

  Future<String> executeFsWrite(String? sandboxRoot, Map<String, dynamic> args) async {
    final root = _validateRoot(sandboxRoot);
    if (root == null) {
      return 'Error: no folder is attached to this conversation. Ask the user to attach one.';
    }

    final op = (args['op'] as String?) ?? '';
    final relPath = (args['path'] as String?) ?? '';
    if (op.isEmpty) return "Error: 'op' is required.";
    if (relPath.isEmpty) return "Error: 'path' is required.";

    final resolved = _resolveInside(root, relPath);
    if (resolved == null) {
      return 'Error: path is outside the attached folder.';
    }

    switch (op) {
      case 'create':
        final content = (args['content'] as String?) ?? '';
        return _create(resolved, content);
      case 'edit':
        final oldText = (args['old_text'] as String?) ?? '';
        final newText = (args['new_text'] as String?) ?? '';
        if (oldText.isEmpty) return "Error: 'old_text' is required for edit.";
        return _edit(resolved, oldText, newText);
      case 'copy':
      case 'move':
        final destRel = (args['dest_path'] as String?) ?? '';
        if (destRel.isEmpty) return "Error: 'dest_path' is required for $op.";
        final destResolved = _resolveInside(root, destRel);
        if (destResolved == null) {
          return 'Error: dest_path is outside the attached folder.';
        }
        return op == 'copy'
            ? _copy(resolved, destResolved)
            : _move(resolved, destResolved);
      case 'delete':
        return _delete(resolved);
      case 'mkdir':
        return _mkdir(resolved);
      default:
        return 'Error: unknown op "$op".';
    }
  }

  // --- path safety ---

  String? _validateRoot(String? root) {
    if (root == null || root.isEmpty) return null;
    // Refuse to accept a relative sandbox root — `path.canonicalize` would
    // silently fall back to the current working directory (on Windows
    // desktop that's typically C:\Users\<user>), leaking the user's home
    // dir as the sandbox.
    if (!path.isAbsolute(root)) return null;
    final dir = Directory(root);
    if (!dir.existsSync()) return null;
    return path.normalize(root);
  }

  /// Returns the absolute path if [relPath] resolves inside [root], otherwise
  /// null. Rejects '..' segments and absolute paths. Uses [path.normalize]
  /// rather than [path.canonicalize] so the resolver never falls back to the
  /// current working directory.
  String? _resolveInside(String root, String relPath) {
    // Treat '.' (and a couple of trivially-equivalent forms) as the root
    // itself — no join/normalize round-trip needed.
    if (relPath.isEmpty || relPath == '.' || relPath == './' || relPath == r'.\') {
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

  // --- read ops ---

  String _list(String root, String dirPath, bool recursive) {
    final dir = Directory(dirPath);
    if (!dir.existsSync()) return 'Error: directory does not exist.';
    final entries = <String>[];
    var truncated = false;
    if (recursive) {
      try {
        for (final e in dir.listSync(recursive: true, followLinks: false)) {
          final rel = path.relative(e.path, from: root);
          final marker = e is Directory ? 'd' : 'f';
          entries.add('$marker $rel');
          if (entries.length >= _maxListEntries) {
            truncated = true;
            break;
          }
        }
      } catch (e) {
        return 'Error listing: $e';
      }
    } else {
      try {
        for (final e in dir.listSync(followLinks: false)) {
          final marker = e is Directory ? 'd' : 'f';
          entries.add('$marker ${path.basename(e.path)}');
          if (entries.length >= _maxListEntries) {
            truncated = true;
            break;
          }
        }
      } catch (e) {
        return 'Error listing: $e';
      }
    }
    entries.sort();
    final header =
        'Listing of ${path.relative(dirPath, from: root)} (${entries.length} entries${truncated ? ", truncated" : ""}):\n';
    return header + entries.join('\n') + '\n';
  }

  String _find(String root, String dirPath, String pattern) {
    final dir = Directory(dirPath);
    if (!dir.existsSync()) return 'Error: directory does not exist.';
    final glob = _globToRegExp(pattern);
    final matches = <String>[];
    var truncated = false;
    try {
      for (final e in dir.listSync(recursive: true, followLinks: false)) {
        if (glob.hasMatch(path.basename(e.path))) {
          matches.add(path.relative(e.path, from: root));
          if (matches.length >= _maxFindResults) {
            truncated = true;
            break;
          }
        }
      }
    } catch (e) {
      return 'Error searching: $e';
    }
    matches.sort();
    final header =
        'Found ${matches.length} match(es) for pattern "$pattern" under ${path.relative(dirPath, from: root)}${truncated ? " (truncated)" : ""}:\n';
    return header + matches.join('\n') + '\n';
  }

  RegExp _globToRegExp(String glob) {
    final sb = StringBuffer('^');
    for (final c in glob.split('')) {
      switch (c) {
        case '*':
          sb.write('.*');
          break;
        case '?':
          sb.write('.');
          break;
        case '.':
        case '(':
        case ')':
        case '+':
        case '|':
        case '^':
        case r'$':
        case '{':
        case '}':
        case '\\':
          sb.write('\\$c');
          break;
        default:
          sb.write(c);
      }
    }
    sb.write(r'$');
    return RegExp(sb.toString());
  }

  String _read(String filePath) {
    final f = File(filePath);
    if (!f.existsSync()) return 'Error: file does not exist.';
    try {
      final bytes = f.readAsBytesSync();
      final truncated = bytes.length > _maxReadBytes;
      final slice = truncated ? bytes.sublist(0, _maxReadBytes) : bytes;
      var content = utf8.decode(slice, allowMalformed: true);
      if (truncated) {
        content +=
            '\n\n[Content truncated — file exceeds $_maxReadBytes bytes]';
      }
      return content;
    } catch (e) {
      return 'Error reading: $e';
    }
  }

  String _readSegment(String filePath, int start, int end) {
    if (start <= 0) start = 1;
    final f = File(filePath);
    if (!f.existsSync()) return 'Error: file does not exist.';
    try {
      final lines = f.readAsLinesSync();
      final actualEnd = (end <= 0 || end > lines.length) ? lines.length : end;
      if (start > lines.length) {
        return 'Lines $start..$actualEnd: (file has only ${lines.length} lines).';
      }
      final slice = lines.sublist(start - 1, actualEnd).join('\n');
      var out = slice;
      if (out.length > _maxReadBytes) {
        out = out.substring(0, _maxReadBytes) +
            '\n[Truncated — segment exceeds $_maxReadBytes bytes]';
      }
      return 'Lines $start..$actualEnd of ${path.basename(filePath)}:\n$out';
    } catch (e) {
      return 'Error reading: $e';
    }
  }

  String _stat(String filePath) {
    final type = FileSystemEntity.typeSync(filePath, followLinks: false);
    if (type == FileSystemEntityType.notFound) return 'Error: not found.';
    try {
      final stat = FileStat.statSync(filePath);
      final kind = type == FileSystemEntityType.directory ? 'directory' : 'file';
      final out = {
        'name': path.basename(filePath),
        'kind': kind,
        'size': stat.size,
        'mode': stat.modeString(),
        'modified': stat.modified.toIso8601String(),
      };
      return const JsonEncoder.withIndent('  ').convert(out);
    } catch (e) {
      return 'Error: $e';
    }
  }

  // --- write ops ---

  String _create(String filePath, String content) {
    final f = File(filePath);
    if (f.existsSync()) {
      return 'Error: "$filePath" already exists. Use op=edit to modify.';
    }
    try {
      Directory(path.dirname(filePath)).createSync(recursive: true);
      f.writeAsStringSync(content);
      return 'Created ${path.basename(filePath)} (${content.length} bytes)';
    } catch (e) {
      return 'Error writing: $e';
    }
  }

  String _edit(String filePath, String oldText, String newText) {
    final f = File(filePath);
    if (!f.existsSync()) return 'Error: file does not exist.';
    try {
      final body = f.readAsStringSync();
      final count = oldText.allMatches(body).length;
      if (count == 0) return "Error: 'old_text' was not found in the file.";
      if (count > 1) {
        return "Error: 'old_text' occurs $count times. Provide more surrounding context so it matches exactly once.";
      }
      final updated = body.replaceFirst(oldText, newText);
      f.writeAsStringSync(updated);
      return 'Edited ${path.basename(filePath)} (${updated.length} bytes after change)';
    } catch (e) {
      return 'Error: $e';
    }
  }

  String _copy(String src, String dst) {
    final type = FileSystemEntity.typeSync(src, followLinks: false);
    if (type == FileSystemEntityType.notFound) return 'Error: source does not exist.';
    try {
      Directory(path.dirname(dst)).createSync(recursive: true);
      if (type == FileSystemEntityType.directory) {
        _copyDir(Directory(src), Directory(dst));
      } else {
        File(src).copySync(dst);
      }
      return 'Copied $src → $dst';
    } catch (e) {
      return 'Error copying: $e';
    }
  }

  void _copyDir(Directory src, Directory dst) {
    dst.createSync(recursive: true);
    for (final entity in src.listSync(followLinks: false)) {
      final newPath = path.join(dst.path, path.basename(entity.path));
      if (entity is Directory) {
        _copyDir(entity, Directory(newPath));
      } else if (entity is File) {
        entity.copySync(newPath);
      }
    }
  }

  String _move(String src, String dst) {
    final type = FileSystemEntity.typeSync(src, followLinks: false);
    if (type == FileSystemEntityType.notFound) return 'Error: source does not exist.';
    try {
      Directory(path.dirname(dst)).createSync(recursive: true);
      if (type == FileSystemEntityType.directory) {
        Directory(src).renameSync(dst);
      } else {
        File(src).renameSync(dst);
      }
      return 'Moved $src → $dst';
    } catch (e) {
      return 'Error: $e';
    }
  }

  String _delete(String filePath) {
    final type = FileSystemEntity.typeSync(filePath, followLinks: false);
    if (type == FileSystemEntityType.notFound) return 'Error: not found.';
    try {
      if (type == FileSystemEntityType.directory) {
        final dir = Directory(filePath);
        if (dir.listSync(followLinks: false).isNotEmpty) {
          return 'Error: directory is not empty. Refusing to delete.';
        }
        dir.deleteSync();
      } else {
        File(filePath).deleteSync();
      }
      return 'Deleted ${path.basename(filePath)}';
    } catch (e) {
      return 'Error: $e';
    }
  }

  String _mkdir(String dirPath) {
    try {
      Directory(dirPath).createSync(recursive: true);
      return 'Created directory ${path.basename(dirPath)}';
    } catch (e) {
      return 'Error: $e';
    }
  }

  int _asInt(dynamic v) {
    if (v == null) return 0;
    if (v is int) return v;
    if (v is double) return v.toInt();
    if (v is String) return int.tryParse(v) ?? 0;
    return 0;
  }
}
