import 'dart:convert';
import 'dart:typed_data';
import 'package:http/http.dart' as http;
import '../api/api.dart';
import '../auth/auth-service.dart';

/// In-memory cache keyed by URL to avoid redundant fetches on widget rebuilds.
final Map<String, Future<Uint8List>> _cache = {};

/// Load image bytes from either an internal attachment URL or a data URI.
/// Results are cached in memory so repeated calls (e.g. StatelessWidget
/// rebuilds, list scrolling) don't trigger redundant network requests.
Future<Uint8List> loadImageBytes(String url) {
  return _cache.putIfAbsent(url, () => _fetchImageBytes(url));
}

Future<Uint8List> _fetchImageBytes(String url) async {
  if (url.startsWith('data:')) {
    return base64Decode(url.split(',').last);
  }
  final token = await AuthService().getCurrentUserToken();
  final response = await http.get(
    Uri.parse('${ApiService.baseUrl}$url'),
    headers: {'Authorization': 'Bearer $token'},
  );
  if (response.statusCode != 200) {
    throw Exception('Failed to load image: ${response.statusCode}');
  }
  return response.bodyBytes;
}
