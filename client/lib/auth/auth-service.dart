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

  static String get baseUrl =>
      kReleaseMode ? 'https://app.plurality-ai.com' : 'http://192.168.1.102:8090';

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

  Future<AuthMethods> getAuthMethods() async {
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
    return AuthMethods(local: true, openid: false);
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

  /// No-op shim kept for compatibility with code that used to refresh the
  /// Firebase ID token after email verification.
  Future<void> forceRefresh() async {}

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
