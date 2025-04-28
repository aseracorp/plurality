import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart' show rootBundle;
import 'dart:convert';
import '../api/stub_reload_helper.dart'
    // If dart.library.html is available (meaning we are compiling for web),
    // import the web-specific implementation instead.
    if (dart.library.html) '../api/web_reload_helper.dart';

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

Future<bool> checkVersion() async {
  final uri = Uri.parse(
    kIsWeb
        ? '/version.json?cacheBust=${DateTime.now().millisecondsSinceEpoch}'
        : 'https://app.plurality-ai.com/version.json?cacheBust=${DateTime.now().millisecondsSinceEpoch}',
  );

  print('Fetching version info from: $uri');

  final response = await http.get(uri);

  if (response.statusCode == 200) {
    final data = json.decode(response.body);
    final latestVersion = data['version'] as String?;

    // store value in shared preferences

    final String cvJsonString = await rootBundle.loadString(
      'assets/version.json',
    );
    print('version.json : ' + cvJsonString);
    final cvJson = json.decode(cvJsonString);
    var currentVersion = cvJson['version'] as String?;
    print('Current version: $currentVersion');
    print('Latest version: $latestVersion');

    if (currentVersion == "" || currentVersion != latestVersion) {
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
