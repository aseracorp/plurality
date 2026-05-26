import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;
import 'package:shared_preferences/shared_preferences.dart';

import 'openid_signin.dart';

class User {
  final String username;
  User({required this.username});
}

class AuthMethods {
  final bool local;
  final bool openid;
  final String? openidIssuer;
  final String? openidClientId;
  AuthMethods({
    required this.local,
    required this.openid,
    this.openidIssuer,
    this.openidClientId,
  });

  bool get openidReady =>
      openid && (openidIssuer ?? '').isNotEmpty && (openidClientId ?? '').isNotEmpty;
}

class AuthService {
  static final AuthService _instance = AuthService._internal();
  factory AuthService() => _instance;
  AuthService._internal();

  static const _tokenKey = 'plurality_jwt';
  static const _usernameKey = 'plurality_username';
  static const _serverUrlKey = 'plurality_server_url';

  static String _nativeServerUrl = '';

  /// Base URL for all backend requests.
  /// - Web: the origin that served the JS, so the app always talks to its own host.
  /// - Native: a user-supplied URL persisted in SharedPreferences (empty until set).
  static String get baseUrl => kIsWeb ? Uri.base.origin : _nativeServerUrl;

  /// True when [baseUrl] is usable. On web this is always true; on native it
  /// requires the user to have set a server URL via the login screen.
  static bool get isConfigured => kIsWeb || _nativeServerUrl.isNotEmpty;

  /// Current native server URL (empty on web or when unset).
  static String get nativeServerUrl => _nativeServerUrl;

  /// Load the saved native server URL into memory. Must be awaited from main()
  /// before any code reads [baseUrl].
  static Future<void> loadServerUrl() async {
    if (kIsWeb) return;
    final prefs = await SharedPreferences.getInstance();
    _nativeServerUrl = prefs.getString(_serverUrlKey) ?? '';
  }

  /// Persist a new native server URL. Trims whitespace and strips a trailing
  /// slash. If the URL is changing, any cached token for the previous server is
  /// wiped so we don't leak credentials across servers.
  static Future<void> setServerUrl(String url) async {
    if (kIsWeb) return;
    var normalized = url.trim();
    while (normalized.endsWith('/')) {
      normalized = normalized.substring(0, normalized.length - 1);
    }
    if (normalized == _nativeServerUrl) return;
    final prefs = await SharedPreferences.getInstance();
    await prefs.remove(_tokenKey);
    await prefs.remove(_usernameKey);
    await prefs.setString(_serverUrlKey, normalized);
    _nativeServerUrl = normalized;
  }

  final StreamController<User?> _stateController =
      StreamController<User?>.broadcast();
  User? _current;
  bool _bootstrapped = false;

  /// Auth-state stream — emits null when logged out, a User when logged in.
  Stream<User?> get authStateChanges async* {
    if (!_bootstrapped) {
      await _bootstrap();
    }
    yield _current;
    yield* _stateController.stream;
  }

  User? get currentUser => _current;

  Future<void> _bootstrap() async {
    _bootstrapped = true;
    if (!isConfigured) {
      _current = null;
      return;
    }
    final prefs = await SharedPreferences.getInstance();
    final token = prefs.getString(_tokenKey);
    final username = prefs.getString(_usernameKey);
    if (token == null || username == null) {
      _current = null;
      return;
    }
    // Probe /auth/me to confirm the token is still valid.
    try {
      final resp = await http.get(
        Uri.parse('$baseUrl/auth/me'),
        headers: {'Authorization': 'Bearer $token'},
      );
      if (resp.statusCode == 200) {
        _current = User(username: username);
      } else {
        await prefs.remove(_tokenKey);
        await prefs.remove(_usernameKey);
        _current = null;
      }
    } catch (_) {
      // Network down on startup — assume the cached token is still good so
      // the user can keep working offline against a known server.
      _current = User(username: username);
    }
  }

  Future<void> _store(String token, String username) async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setString(_tokenKey, token);
    await prefs.setString(_usernameKey, username);
    _current = User(username: username);
    _stateController.add(_current);
  }

  /// Sign in with username + password. Throws on failure.
  Future<User> signInWithEmailPassword(String username, String password) async {
    final resp = await http.post(
      Uri.parse('$baseUrl/auth/login'),
      headers: {'Content-Type': 'application/json'},
      body: jsonEncode({'username': username, 'password': password}),
    );
    if (resp.statusCode != 200) {
      throw AuthException(_decodeError(resp));
    }
    final data = jsonDecode(resp.body) as Map<String, dynamic>;
    final token = data['token'] as String;
    final canonical = (data['username'] as String?) ?? username;
    await _store(token, canonical);
    return _current!;
  }

  /// Ask the server which login methods it supports. Returns null when the
  /// server can't be reached or replies with an error, so callers can tell
  /// "unreachable server" apart from "server has no local users".
  Future<AuthMethods?> getAuthMethods() async {
    try {
      final resp = await http.get(Uri.parse('$baseUrl/auth/methods'));
      if (resp.statusCode == 200) {
        final data = jsonDecode(resp.body) as Map<String, dynamic>;
        return AuthMethods(
          local: data['local'] == true,
          openid: data['openid'] == true,
          openidIssuer: data['openid_issuer'] as String?,
          openidClientId: data['openid_client_id'] as String?,
        );
      }
    } catch (_) {}
    return null;
  }

  /// Run the openid_client flow, then exchange the resulting ID token for a
  /// Plurality JWT via POST /auth/openid/exchange.
  Future<User> signInWithOpenID(AuthMethods methods) async {
    if (!methods.openidReady) {
      throw AuthException('OpenID is not configured on this server');
    }
    final idToken = await getOpenIDIdToken(
      issuer: methods.openidIssuer!,
      clientId: methods.openidClientId!,
    );
    final resp = await http.post(
      Uri.parse('$baseUrl/auth/openid/exchange'),
      headers: {'Content-Type': 'application/json'},
      body: jsonEncode({'id_token': idToken}),
    );
    if (resp.statusCode != 200) {
      throw AuthException(_decodeError(resp));
    }
    final data = jsonDecode(resp.body) as Map<String, dynamic>;
    final token = data['token'] as String;
    final username = data['username'] as String;
    await _store(token, username);
    return _current!;
  }

  Future<String?> getCurrentUserToken() async {
    if (!_bootstrapped) {
      await _bootstrap();
    }
    final prefs = await SharedPreferences.getInstance();
    return prefs.getString(_tokenKey);
  }

  Future<void> changePassword(String oldPw, String newPw) async {
    final token = await getCurrentUserToken();
    if (token == null) throw AuthException('not signed in');
    final resp = await http.post(
      Uri.parse('$baseUrl/auth/change-password'),
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer $token',
      },
      body: jsonEncode({'old_password': oldPw, 'new_password': newPw}),
    );
    if (resp.statusCode != 204 && resp.statusCode != 200) {
      throw AuthException(_decodeError(resp));
    }
  }

  Future<void> signOut() async {
    final token = await getCurrentUserToken();
    final prefs = await SharedPreferences.getInstance();
    await prefs.remove(_tokenKey);
    await prefs.remove(_usernameKey);
    _current = null;
    _stateController.add(null);
    if (token != null) {
      try {
        await http.post(
          Uri.parse('$baseUrl/auth/logout'),
          headers: {'Authorization': 'Bearer $token'},
        );
      } catch (_) {}
    }
  }

  bool isUserLoggedIn() => _current != null;

  String _decodeError(http.Response resp) {
    final body = resp.body.trim();
    if (body.isEmpty) return 'request failed (${resp.statusCode})';
    try {
      final json = jsonDecode(body);
      if (json is Map && json['error'] is String) return json['error'] as String;
      if (json is Map && json['message'] is String) return json['message'] as String;
    } catch (_) {}
    return body;
  }
}

class AuthException implements Exception {
  final String message;
  AuthException(this.message);
  @override
  String toString() => message;
}
