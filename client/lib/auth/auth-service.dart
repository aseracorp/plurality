import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;
import 'package:shared_preferences/shared_preferences.dart';

import 'openid_signin.dart';
import '../api/models_service.dart';

class User {
  final String username;
  User({required this.username});
}

class AuthMethods {
  final bool local;
  final bool openid;
  final String? openidName;
  final String? openidIssuer;
  final String? openidClientId;
  final String? openidBtnColor;
  final String? openidBtnBg1;
  final String? openidBtnBg2;
  AuthMethods({
    required this.local,
    required this.openid,
    this.openidName,
    this.openidIssuer,
    this.openidClientId,
    this.openidBtnColor,
    this.openidBtnBg1,
    this.openidBtnBg2,
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

  /// Optional compile-time override for the backend origin. Set it for local dev
  /// when the server runs separately from the web client, e.g.:
  ///   flutter run -d chrome --dart-define=API_BASE_URL=http://localhost:8080
  /// Empty (the default) falls back to the origin that served the app.
  static const _apiBaseUrlOverride =
      String.fromEnvironment('API_BASE_URL', defaultValue: '');

  /// Base URL for all backend requests.
  /// - Web: the API_BASE_URL override if set, else the origin that served the
  ///   JS, so the app always talks to its own host.
  /// - Native: a user-supplied URL persisted in SharedPreferences (empty until set).
  static String get baseUrl => kIsWeb
      ? (_apiBaseUrlOverride.isNotEmpty ? _apiBaseUrlOverride : Uri.base.origin)
      : _nativeServerUrl;

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

  /// The last error from completing an OpenID redirect, if any. Set when a
  /// provider redirect comes back but the login can't be finished (clobbered
  /// fragment, state mismatch, or a server rejection like allowlist/userinfo).
  /// The login screen reads this once to show the real reason, then clears it.
  String? lastOpenIDError;

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
    print('[OpenID] _bootstrap: isConfigured=$isConfigured baseUrl=$baseUrl');
    if (!isConfigured) {
      _current = null;
      return;
    }
    final prefs = await SharedPreferences.getInstance();
    final token = prefs.getString(_tokenKey);
    final username = prefs.getString(_usernameKey);
    print('[OpenID] _bootstrap: cachedToken=${token != null} '
        'cachedUser=${username != null}');
    if (token == null || username == null) {
      // We may have just been redirected back from an OpenID provider with the
      // id_token in the URL fragment (web). Pick it up and finish the login.
      if (await _tryCompleteOpenIDRedirect()) {
        print('[OpenID] _bootstrap: OpenID redirect completed, session established');
        return;
      }
      print('[OpenID] _bootstrap: no OpenID session established → logged out');
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
      print('[OpenID] getAuthMethods: GET $baseUrl/auth/methods → ${resp.statusCode}');
      if (resp.statusCode == 200) {
        final data = jsonDecode(resp.body) as Map<String, dynamic>;
        return AuthMethods(
          local: data['local'] == true,
          openid: data['openid'] == true,
          openidName: data['openid_name'] as String?,
          openidIssuer: data['openid_issuer'] as String?,
          openidClientId: data['openid_client_id'] as String?,
          openidBtnColor: data['openid_btn_color'] as String?,
          openidBtnBg1: data['openid_btn_bg1'] as String?,
          openidBtnBg2: data['openid_btn_bg2'] as String?,
        );
      }
    } catch (e) {
      print('[OpenID] getAuthMethods: request failed → $e');
    }
    return null;
  }

  /// Run the openid_client flow, then exchange the resulting ID token for a
  /// Plurality JWT via POST /auth/openid/exchange.
  Future<User> signInWithOpenID(AuthMethods methods) async {
    print('[OpenID] signInWithOpenID: starting flow '
        'issuer=${methods.openidIssuer} clientId=${methods.openidClientId}');
    if (!methods.openidReady) {
      throw AuthException('OpenID is not configured on this server');
    }
    final result = await getOpenIDIdToken(
      issuer: methods.openidIssuer!,
      clientId: methods.openidClientId!,
    );
    return _exchangeOpenIDToken(result);
  }

  /// On web startup, check whether the provider just redirected us back with an
  /// id_token in the URL fragment; if so, exchange it and complete the login.
  /// Returns true when a session was established. No-op on other platforms.
  Future<bool> _tryCompleteOpenIDRedirect() async {
    lastOpenIDError = null;
    final methods = await getAuthMethods();
    print('[OpenID] _tryCompleteOpenIDRedirect: methods=${methods == null ? 'null (server unreachable?)' : 'loaded'} '
        'openidReady=${methods?.openidReady}');
    if (methods == null || !methods.openidReady) {
      return false;
    }
    try {
      final result = await completeOpenIDRedirect(
        issuer: methods.openidIssuer!,
        clientId: methods.openidClientId!,
      );
      if (result == null) {
        print('[OpenID] _tryCompleteOpenIDRedirect: no redirect in progress');
        return false;
      }
      print('[OpenID] _tryCompleteOpenIDRedirect: got redirect result, exchanging with server');
      await _exchangeOpenIDToken(result);
      print('[OpenID] _tryCompleteOpenIDRedirect: exchange succeeded, logged in');
      return true;
    } catch (e) {
      // Surface the real reason instead of silently dropping back to the login
      // form. _exchangeOpenIDToken throws AuthException carrying the server's
      // error body (e.g. allowlist/userinfo), and completeOpenIDRedirect throws
      // a descriptive message when the redirect response is lost.
      print('[OpenID] _tryCompleteOpenIDRedirect: FAILED → $e');
      lastOpenIDError = e.toString();
      return false;
    }
  }

  /// Exchange an OpenID sign-in result for a Plurality JWT and store the
  /// session. Native sends an id_token (+ access token for the userinfo
  /// fallback); web sends an authorization code + PKCE verifier and the server
  /// performs the token exchange.
  Future<User> _exchangeOpenIDToken(OpenIDResult result) async {
    print('[OpenID] exchange: POST $baseUrl/auth/openid/exchange '
        '(mode=${result.code != null ? 'code+PKCE' : 'id_token'}, '
        'hasAccessToken=${result.accessToken != null})');
    final resp = await http.post(
      Uri.parse('$baseUrl/auth/openid/exchange'),
      headers: {'Content-Type': 'application/json'},
      body: jsonEncode({
        if (result.idToken != null) 'id_token': result.idToken,
        if (result.accessToken != null) 'access_token': result.accessToken,
        if (result.code != null) 'code': result.code,
        if (result.codeVerifier != null) 'code_verifier': result.codeVerifier,
        if (result.redirectUri != null) 'redirect_uri': result.redirectUri,
      }),
    );
    print('[OpenID] exchange: server responded ${resp.statusCode} '
        'body="${resp.body.length > 200 ? '${resp.body.substring(0, 200)}…' : resp.body}"');
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
    // Drop the in-memory model cache so the next login re-fetches from the
    // (possibly different) server instead of reusing the old session's list.
    ModelsService().invalidate();
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
