// Web logger for startup crash-guards. There is no file system on web, so
// errors are surfaced on the developer console instead.
import 'package:flutter/foundation.dart';

String crashLogDir = '';

void writeCrashLog(String source, Object error, StackTrace? stack) {
  debugPrint('[$source] $error\n$stack');
}