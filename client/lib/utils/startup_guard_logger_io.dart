// Native (io) logger for startup crash-guards. Appends synchronously to
// <applicationSupport>/plurality_crash.log so the entry survives even if
// the isolate is about to die — a synchronous write must not rely on the
// event loop.
import 'dart:io';

String crashLogDir = '';

void writeCrashLog(String source, Object error, StackTrace? stack) {
  try {
    final dir = crashLogDir;
    final file = File('$dir${Platform.pathSeparator}plurality_crash.log');
    file.writeAsStringSync(
      '[$source] ${DateTime.now().toIso8601String()}\n'
      '$error\n'
      '${stack ?? ''}\n'
      '---\n',
      mode: FileMode.append,
    );
  } catch (_) {
    // Logging must never be the thing that crashes the app.
  }
}