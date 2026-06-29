import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart' show rootBundle;
import 'dart:convert';
import 'dart:io' show Platform;
import '../api/stub_reload_helper.dart'
    // If dart.library.html is available (meaning we are compiling for web),
    // import the web-specific implementation instead.
    if (dart.library.html) '../api/web_reload_helper.dart';

/// Formats a namespaced tool name for display: "server__navigate" → "server(navigate)".
/// Non-namespaced names are returned as-is.
String formatToolDisplayName(String name) {
  final idx = name.indexOf('__');
  if (idx < 0) return name;
  return '${name.substring(0, idx)}(${name.substring(idx + 2)})';
}

String sanitizeMessages(String message) {
  return message.replaceAll(
    RegExp(
      r'<hidden>((.|\n)*)</hidden>',
      multiLine: true,
      caseSensitive: false,
    ),
    '',
  );
}

/// Compares two semantic version strings (e.g. "1.2.3").
///
/// Returns a negative number if [a] < [b], zero if they are equal, and a
/// positive number if [a] > [b]. Any pre-release/build suffix (after `-` or
/// `+`) is stripped before comparison, and missing components are treated as 0
/// so that "1.2" == "1.2.0". Non-numeric components compare as 0.
int compareSemver(String a, String b) {
  List<int> parse(String v) {
    final core = v.trim().split(RegExp(r'[-+]')).first;
    return core
        .split('.')
        .map((part) => int.tryParse(part.trim()) ?? 0)
        .toList();
  }

  final pa = parse(a);
  final pb = parse(b);
  final length = pa.length > pb.length ? pa.length : pb.length;

  for (var i = 0; i < length; i++) {
    final na = i < pa.length ? pa[i] : 0;
    final nb = i < pb.length ? pb[i] : 0;
    if (na != nb) return na - nb;
  }
  return 0;
}

/// Whether [latest] is a strictly newer version than [current].
bool isNewerVersion(String latest, String current) {
  if (current.isEmpty) return true;
  if (latest.isEmpty) return false;
  return compareSemver(latest, current) > 0;
}

/// Maps the current native platform to its update-check slug.
/// Returns null on web or any unsupported platform.
String? _updateCheckPlatform() {
  if (kIsWeb) return null;
  if (Platform.isAndroid) return 'android';
  if (Platform.isLinux) return 'linux';
  if (Platform.isMacOS) return 'macos';
  if (Platform.isWindows) return 'windows';
  return null;
}

Future<bool> checkVersion() async {
  final cacheBust = DateTime.now().millisecondsSinceEpoch;
  final String url;
  if (kIsWeb) {
    url = '/version.json?cacheBust=$cacheBust';
  } else {
    final platform = _updateCheckPlatform();
    if (platform == null) {
      print('Version check skipped: unsupported platform');
      return false;
    }
    url = 'https://cosmos-cloud.io/update-check/plurality/$platform?cacheBust=$cacheBust';
  }
  final uri = Uri.parse(url);

  print('Fetching version info from: $uri');

  final response = await http.get(uri);

  if (response.statusCode == 200) {
    final data = json.decode(response.body);
    // The native update-check endpoint returns {"latest": "x.y.z", "link": ...};
    // the web build's bundled /version.json uses {"version": "x.y.z"}.
    final latestVersion = (data['latest'] ?? data['version']) as String?;

    // store value in shared preferences

    final String cvJsonString = await rootBundle.loadString(
      'assets/version.json',
    );
    print('version.json : ' + cvJsonString);
    final cvJson = json.decode(cvJsonString);
    var currentVersion = cvJson['version'] as String?;
    print('Current version: $currentVersion');
    print('Latest version: $latestVersion');

    if (isNewerVersion(latestVersion ?? '', currentVersion ?? '')) {
      print('New version available: $latestVersion');
      if (kIsWeb) {
        platformSpecificReload();
      }
      return true;
    } else {
      print('No new version available');
    }
  }

  return false;
}
