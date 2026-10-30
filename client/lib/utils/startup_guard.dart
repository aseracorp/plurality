import 'dart:async';

import 'package:flutter/foundation.dart';
import 'dart:ui' show PlatformDispatcher;

// Conditional import, mirroring the repo's web_visibility pattern: the web
// build gets the console logger (no dart:io), native builds get the file
// logger.
import 'startup_guard_logger_web.dart'
    if (dart.library.io) 'startup_guard_logger_io.dart';

/// Startup crash-guard for the native desktop app.
///
/// The Windows client (and other desktop targets) used to start with a blank
/// window or crash outright when anything in the async startup path threw an
/// unhandled exception in the root zone:
///   - `checkVersion()` performs a plain `http.get()` to the update server;
///     on managed/corporate networks (web proxy, endpoint security TLS
///     interception) that request can fail with a SocketException or
///     HttpException right during the first frame.
///   - `MCPService().initMCP()` reads + JSON-decodes `mcp.json` and spawns
///     helper processes (npx/node/python) at startup. A corrupt file or a
///     blocked/broken helper binary (e.g. blocked by EDR) used to surface as
///     an unhandled exception.
///   - Hive init can hit a stale `.lock` / corrupt box after a hard crash.
///
/// None of those should ever take the whole app (and the user's first frame)
/// down. This file moves the app into a guarded zone, logs every uncaught
/// error (to disk on native, console on web) and — critically — does not
/// rethrow, so a late async failure can no longer blank or kill the UI.
///
/// The log file also turns the classic "blank window / crash on start"
/// report into something diagnosable on managed endpoints where console
/// output is invisible.

/// Installs global error handlers. Call once from `main()`, after
/// `WidgetsFlutterBinding.ensureInitialized()`, inside the guarded zone
/// (see `main.dart`).
void installStartupCrashGuards() {
  // Flutter framework errors (build/layout/etc.) — log, don't tear down.
  final prevFlutterError = FlutterError.onError;
  FlutterError.onError = (details) {
    writeCrashLog('FlutterError', details.exception, details.stack);
    if (prevFlutterError != null) {
      // Keep the default behaviour (it prints to console in debug).
      prevFlutterError(details);
    }
  };

  // Errors on the platform/UI isolate that would otherwise terminate the app.
  final prevPlatformError = PlatformDispatcher.instance.onError;
  PlatformDispatcher.instance.onError = (error, stack) {
    writeCrashLog('PlatformDispatcher', error, stack);
    if (prevPlatformError != null) {
      return prevPlatformError(error, stack);
    }
    // Return true: the error was handled, keep the app alive.
    return true;
  };
}

/// Root-zone error handler for `runZonedGuarded`. Logs and swallows so the
/// app survives late one-off failures (network, plugins, background tasks).
void onRootZoneError(Object error, StackTrace stack) {
  writeCrashLog('root-zone', error, stack);
}

/// Wraps an async startup task so a failure is logged and swallowed instead
/// of aborting the rest of startup (which would leave a blank window or no
/// window at all). Returns null on failure.
Future<T?> guarded<T>(Future<T> Function() fn, {String? label}) async {
  try {
    return await fn();
  } catch (e, st) {
    writeCrashLog(label == null ? 'guarded' : 'guarded:$label', e, st);
    return null;
  }
}

/// Sets the directory for the on-disk crash log. Call once at startup with
/// the application-support directory (the same folder Hive uses) so the log
/// lives next to the app data it diagnoses.
void setCrashLogDirectory(String directory) {
  crashLogDir = directory;
}