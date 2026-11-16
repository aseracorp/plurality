import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

import 'package:plurality/api/filesystem_ops.dart';
import 'package:plurality/api/filesystem_service.dart';

/// The device filesystem tool used to run every syscall — `listSync`,
/// `readAsBytesSync`, `writeAsStringSync`, `existsSync` — on the isolate that
/// dispatches client tools. That is the UI isolate, so one call against a
/// sleeping Tailscale/SMB/OneDrive share parked it forever: the window froze,
/// no tool result was sent, the server's 90s watchdog synthesized a timeout,
/// and every device call after it timed out too.
///
/// The syscall bodies now live at top level in `filesystem_ops.dart` so
/// `compute()` can run them on a worker isolate, and the path safety rules are
/// pure. These cover the bodies (the sandbox invariants) on a real temp dir.
void main() {
  late Directory tmp;
  setUp(() => tmp = Directory.systemTemp.createTempSync('fs_ops'));
  tearDown(() {
    if (tmp.existsSync()) tmp.deleteSync(recursive: true);
  });

  group('sandbox root + path resolution', () {
    test('a relative root is refused (would leak the cwd)', () {
      expect(FilesystemService.validateRootPath('.'), isNull);
      expect(FilesystemService.validateRootPath('relative/dir'), isNull);
      expect(FilesystemService.validateRootPath(''), isNull);
      expect(FilesystemService.validateRootPath(null), isNull);
    });

    test('an absolute root is normalized', () {
      expect(
        FilesystemService.validateRootPath('${tmp.path}/sub/../'),
        path.normalize('${tmp.path}/sub'),
      );
    });

    test("'.' resolves to the root itself", () {
      final root = FilesystemService.validateRootPath(tmp.path)!;
      expect(FilesystemService.resolveInside(root, '.'), root);
      expect(FilesystemService.resolveInside(root, './'), root);
      expect(FilesystemService.resolveInside(root, ''), root);
    });

    test("'..' and absolute paths are rejected", () {
      final root = FilesystemService.validateRootPath(tmp.path)!;
      expect(FilesystemService.resolveInside(root, '../etc/passwd'), isNull);
      expect(FilesystemService.resolveInside(root, 'a/../../b'), isNull);
      expect(FilesystemService.resolveInside(root, '/etc/passwd'), isNull);
    });

    test('a nested path inside the root resolves', () {
      final root = FilesystemService.validateRootPath(tmp.path)!;
      expect(
        FilesystemService.resolveInside(root, 'a/b.txt'),
        path.join(root, 'a/b.txt'),
      );
    });
  });

  group('op bodies', () {
    test('create / read / edit round-trip', () {
      final p = '${tmp.path}/a.txt';
      expect(fsCreate(p, 'hello world'), startsWith('Created'));
      expect(fsRead(p), 'hello world');
      expect(fsEdit(p, 'world', 'dart'), startsWith('Edited'));
      expect(fsRead(p), 'hello dart');
    });

    test('create refuses to clobber; edit needs a unique match', () {
      final p = '${tmp.path}/b.txt';
      fsCreate(p, 'x x');
      expect(fsCreate(p, 'y'), contains('already exists'));
      expect(fsEdit(p, 'nope', 'z'), contains('was not found'));
      expect(fsEdit(p, 'x', 'z'), contains('occurs 2 times'));
    });

    test('list shallow vs recursive', () {
      File('${tmp.path}/one.dart').writeAsStringSync('a');
      Directory('${tmp.path}/sub').createSync();
      File('${tmp.path}/sub/two.dart').writeAsStringSync('b');
      final shallow = fsList(tmp.path, tmp.path, false);
      expect(shallow, contains('one.dart'));
      expect(shallow.contains('sub/two.dart'), isFalse);
      expect(fsList(tmp.path, tmp.path, true), contains('sub/two.dart'));
    });

    test('find matches a glob across the tree', () {
      File('${tmp.path}/one.dart').writeAsStringSync('a');
      Directory('${tmp.path}/sub').createSync();
      File('${tmp.path}/sub/two.dart').writeAsStringSync('b');
      final out = fsFind(tmp.path, tmp.path, '*.dart');
      expect(out, contains('one.dart'));
      expect(out, contains('sub/two.dart'));
    });

    test('read_segment', () {
      final p = '${tmp.path}/s.txt';
      File(p).writeAsStringSync('l1\nl2\nl3');
      expect(fsReadSegment(p, 2, 3), contains('l2'));
      expect(fsReadSegment(p, 9, 0), contains('only 3 lines'));
    });

    test('stat, mkdir, copy, move, delete', () {
      expect(fsMkdir('${tmp.path}/d'), startsWith('Created directory'));
      expect(fsStat('${tmp.path}/d'), contains('"kind": "directory"'));
      expect(fsStat('${tmp.path}/gone'), 'Error: not found.');
      File('${tmp.path}/m.txt').writeAsStringSync('z');
      expect(
        fsMove('${tmp.path}/m.txt', '${tmp.path}/d/m.txt'),
        startsWith('Moved'),
      );
      expect(
        fsCopy('${tmp.path}/d/m.txt', '${tmp.path}/c.txt'),
        startsWith('Copied'),
      );
      expect(fsDelete('${tmp.path}/d'), contains('not empty'));
      expect(fsDelete('${tmp.path}/c.txt'), startsWith('Deleted'));
    });

    test('runFsOp dispatches by op and rejects unknown ops', () {
      final p = '${tmp.path}/k.txt';
      expect(
        runFsOp(
          FsRequest(op: 'create', root: tmp.path, path: p, content: 'ok'),
        ),
        startsWith('Created'),
      );
      expect(runFsOp(FsRequest(op: 'read', root: tmp.path, path: p)), 'ok');
      expect(
        runFsOp(FsRequest(op: 'bogus', root: tmp.path, path: tmp.path)),
        contains('unknown op'),
      );
    });
  });
}
