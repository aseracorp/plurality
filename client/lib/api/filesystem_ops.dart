import 'dart:convert';
import 'dart:io';

import 'package:path/path.dart' as path;

import '../utils/textsafe.dart';

const int _fsMaxReadBytes = 200 * 1024;
const int _fsMaxListEntries = 500;
const int _fsMaxFindResults = 500;

/// One filesystem request, shipped to a worker isolate.
///
/// A plain data class so `compute()` can send it: it carries only strings,
/// ints and a bool.
class FsRequest {
  final String op;
  final String root;
  final String path;
  final String destPath;
  final String pattern;
  final bool recursive;
  final int startLine;
  final int endLine;
  final String content;
  final String oldText;
  final String newText;

  const FsRequest({
    required this.op,
    required this.root,
    required this.path,
    this.destPath = '',
    this.pattern = '',
    this.recursive = false,
    this.startLine = 0,
    this.endLine = 0,
    this.content = '',
    this.oldText = '',
    this.newText = '',
  });
}

/// Entry point for `compute()`: performs one filesystem operation on a worker
/// isolate and returns the tool-result string. All the syscall work lives at
/// top level so it is isolate-sendable and unit-testable.
String runFsOp(FsRequest req) {
  switch (req.op) {
    case 'list':
      return fsList(req.root, req.path, req.recursive);
    case 'find':
      return fsFind(req.root, req.path, req.pattern);
    case 'read':
      return fsRead(req.path);
    case 'read_segment':
      return fsReadSegment(req.path, req.startLine, req.endLine);
    case 'stat':
      return fsStat(req.path);
    case 'create':
      return fsCreate(req.path, req.content);
    case 'edit':
      return fsEdit(req.path, req.oldText, req.newText);
    case 'copy':
      return fsCopy(req.path, req.destPath);
    case 'move':
      return fsMove(req.path, req.destPath);
    case 'delete':
      return fsDelete(req.path);
    case 'mkdir':
      return fsMkdir(req.path);
    default:
      return 'Error: unknown op "${req.op}".';
  }
}

String fsList(String root, String dirPath, bool recursive) {
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
        if (entries.length >= _fsMaxListEntries) {
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
        if (entries.length >= _fsMaxListEntries) {
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

String fsFind(String root, String dirPath, String pattern) {
  final dir = Directory(dirPath);
  if (!dir.existsSync()) return 'Error: directory does not exist.';
  final glob = globToRegExp(pattern);
  final matches = <String>[];
  var truncated = false;
  try {
    for (final e in dir.listSync(recursive: true, followLinks: false)) {
      if (glob.hasMatch(path.basename(e.path))) {
        matches.add(path.relative(e.path, from: root));
        if (matches.length >= _fsMaxFindResults) {
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

RegExp globToRegExp(String glob) {
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

String fsRead(String filePath) {
  final f = File(filePath);
  if (!f.existsSync()) return 'Error: file does not exist.';
  try {
    final bytes = f.readAsBytesSync();
    final truncated = bytes.length > _fsMaxReadBytes;
    final slice = truncated ? bytes.sublist(0, _fsMaxReadBytes) : bytes;
    var content = utf8.decode(slice, allowMalformed: true);
    if (truncated) {
      content +=
          '\n\n[Content truncated — file exceeds $_fsMaxReadBytes bytes]';
    }
    return content;
  } catch (e) {
    return 'Error reading: $e';
  }
}

String fsReadSegment(String filePath, int start, int end) {
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
    if (out.length > _fsMaxReadBytes) {
      out =
          headGraphemes(out, _fsMaxReadBytes) +
          '\n[Truncated — segment exceeds $_fsMaxReadBytes bytes]';
    }
    return 'Lines $start..$actualEnd of ${path.basename(filePath)}:\n$out';
  } catch (e) {
    return 'Error reading: $e';
  }
}

String fsStat(String filePath) {
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

String fsCreate(String filePath, String content) {
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

String fsEdit(String filePath, String oldText, String newText) {
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

String fsCopy(String src, String dst) {
  final type = FileSystemEntity.typeSync(src, followLinks: false);
  if (type == FileSystemEntityType.notFound)
    return 'Error: source does not exist.';
  try {
    Directory(path.dirname(dst)).createSync(recursive: true);
    if (type == FileSystemEntityType.directory) {
      copyDir(Directory(src), Directory(dst));
    } else {
      File(src).copySync(dst);
    }
    return 'Copied $src → $dst';
  } catch (e) {
    return 'Error copying: $e';
  }
}

void copyDir(Directory src, Directory dst) {
  dst.createSync(recursive: true);
  for (final entity in src.listSync(followLinks: false)) {
    final newPath = path.join(dst.path, path.basename(entity.path));
    if (entity is Directory) {
      copyDir(entity, Directory(newPath));
    } else if (entity is File) {
      entity.copySync(newPath);
    }
  }
}

String fsMove(String src, String dst) {
  final type = FileSystemEntity.typeSync(src, followLinks: false);
  if (type == FileSystemEntityType.notFound)
    return 'Error: source does not exist.';
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

String fsDelete(String filePath) {
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

String fsMkdir(String dirPath) {
  try {
    Directory(dirPath).createSync(recursive: true);
    return 'Created directory ${path.basename(dirPath)}';
  } catch (e) {
    return 'Error: $e';
  }
}
