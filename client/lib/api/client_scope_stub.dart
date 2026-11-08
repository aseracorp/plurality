// Non-web implementation of the scoped-id store.
// Imported on native builds only (conditional import in client_identity.dart).
// Desktop/mobile run a single app process, so a durable per-device value is
// exactly what we want there.
import 'package:shared_preferences/shared_preferences.dart';

Future<String?> readScopedId(String key) async {
  final prefs = await SharedPreferences.getInstance();
  return prefs.getString(key);
}

Future<void> writeScopedId(String key, String value) async {
  final prefs = await SharedPreferences.getInstance();
  await prefs.setString(key, value);
}
