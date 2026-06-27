import 'dart:convert';
import 'dart:html' as html;
import 'dart:math';

import 'package:crypto/crypto.dart';
import 'package:openid_client/openid_client_browser.dart';

import 'openid_result.dart';

// Web uses the OAuth 2.0 Authorization Code flow with PKCE — NOT the implicit
// flow. openid_client_browser's Authenticator only supports the implicit flow
// (response_type=id_token token), which modern providers (Ory, Auth0, Keycloak…)
// forbid: "the OAuth 2.0 Client is not allowed to use the authorization grant
// 'implicit'". So we drive the code+PKCE flow by hand here and let the server do
// the code->token exchange (the provider's token endpoint usually isn't
// CORS-enabled for browser calls). openid_client is used only for discovery.

const _kVerifierKey = 'plurality_oidc_verifier';
const _kStateKey = 'plurality_oidc_state';
const _kRedirectKey = 'plurality_oidc_redirect';

/// PKCE-safe random string (RFC 7636 unreserved characters).
String _randomString(int length) {
  const chars =
      'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~';
  final r = Random.secure();
  return List.generate(length, (_) => chars[r.nextInt(chars.length)]).join();
}

/// S256 code challenge: base64url(sha256(verifier)) without padding.
String _codeChallenge(String verifier) {
  final digest = sha256.convert(ascii.encode(verifier));
  return base64Url.encode(digest.bytes).replaceAll('=', '');
}

/// The redirect URI registered with the provider: the app's origin + path,
/// without any query or fragment. Must be byte-identical between the authorize
/// request and the token exchange, so it is stored at initiate time and reused
/// on callback.
String _redirectUri() {
  final loc = html.window.location;
  final origin = loc.origin ?? '';
  final path = loc.pathname ?? '/';
  return '$origin$path';
}

/// Reads the OAuth response the provider sent back. The Authorization Code flow
/// returns `?code=...&state=...` in the QUERY string (which, unlike the URL
/// fragment, survives Flutter web's hash router). Also checks the fragment and
/// the index.html sessionStorage stash as fallbacks. Returns the raw
/// `key=value&...` string, or null when there is no OAuth response in play.
String? _consumeOAuthResponse() {
  var raw = html.window.sessionStorage['plurality_oauth_fragment'];
  html.window.sessionStorage.remove('plurality_oauth_fragment');
  String source = 'sessionStorage';

  if (raw == null || !_looksLikeOAuthResponse(raw)) {
    final s = html.window.location.search ?? '';
    final q = s.startsWith('?') ? s.substring(1) : s;
    if (_looksLikeOAuthResponse(q)) {
      raw = q;
      source = 'url?query';
    }
  }
  if (raw == null || !_looksLikeOAuthResponse(raw)) {
    final h = html.window.location.hash ?? '';
    final f = h.startsWith('#') ? h.substring(1) : h;
    if (_looksLikeOAuthResponse(f)) {
      raw = f;
      source = 'url#fragment';
    }
  }

  print('[OpenID] consume: source=$source '
      'hash="${html.window.location.hash}" search="${html.window.location.search}"');

  if (raw == null || !_looksLikeOAuthResponse(raw)) return null;
  return raw;
}

bool _looksLikeOAuthResponse(String s) =>
    RegExp(r'(^|&)(code|id_token|access_token|error)=').hasMatch(s);

/// Starts the Authorization Code + PKCE flow: redirects the page to the
/// provider's authorization endpoint and never returns (the await below is just
/// to keep the caller pending until the navigation tears down the page).
/// [completeOpenIDRedirect] picks up the result on the next app load.
Future<OpenIDResult> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
  print('[OpenID] getOpenIDIdToken: discovering issuer=$issuer clientId=$clientId');
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  final authEndpoint = issuerInfo.metadata.authorizationEndpoint;
  print('[OpenID] issuer discovered: authEndpoint=$authEndpoint '
      'responseTypes=${issuerInfo.metadata.responseTypesSupported}');

  final verifier = _randomString(64);
  final state = _randomString(24);
  final nonce = _randomString(24);
  final redirectUri = _redirectUri();

  // Persist what we need to validate and complete the flow after the redirect.
  html.window.sessionStorage[_kVerifierKey] = verifier;
  html.window.sessionStorage[_kStateKey] = state;
  html.window.sessionStorage[_kRedirectKey] = redirectUri;

  final authUrl = authEndpoint.replace(queryParameters: {
    'response_type': 'code',
    'client_id': clientId,
    'redirect_uri': redirectUri,
    'scope': 'openid email profile',
    'state': state,
    'nonce': nonce,
    'code_challenge': _codeChallenge(verifier),
    'code_challenge_method': 'S256',
  });

  print('[OpenID] redirecting to authorize (code+PKCE), redirect_uri=$redirectUri');
  html.window.location.assign(authUrl.toString());
  // The page is being replaced by the provider's login screen.
  await Future.delayed(const Duration(seconds: 30));
  throw Exception('OpenID redirect did not start');
}

/// Picks up an Authorization Code redirect that has already happened. Returns
/// the code + PKCE verifier for the server to exchange, or null when there is no
/// redirect in progress. The server performs the token exchange and verifies
/// the resulting id_token.
Future<OpenIDResult?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async {
  final raw = _consumeOAuthResponse();
  if (raw == null) return null; // normal startup, no redirect to complete

  final params = Uri.splitQueryString(raw);
  print('[OpenID] redirect params: ${params.keys.toList()}');

  final err = params['error'];
  if (err != null) {
    final desc = params['error_description'];
    throw Exception(
        'OpenID provider returned error: $err${desc != null ? ' — $desc' : ''}');
  }

  final code = params['code'];
  if (code == null || code.isEmpty) {
    throw Exception(
        'OpenID redirect contained no authorization code (params: ${params.keys.join(", ")})');
  }

  // CSRF defense: the returned state must match what we stored at initiate time.
  final expectedState = html.window.sessionStorage[_kStateKey];
  final returnedState = params['state'];
  if (expectedState != null &&
      expectedState.isNotEmpty &&
      returnedState != null &&
      returnedState != expectedState) {
    throw Exception('OpenID state mismatch (stale redirect or CSRF)');
  }

  final verifier = html.window.sessionStorage[_kVerifierKey];
  final redirectUri = html.window.sessionStorage[_kRedirectKey] ?? _redirectUri();

  // Consume the one-time flow state and strip the response from the URL so a
  // reload or logout can't replay it.
  html.window.sessionStorage.remove(_kStateKey);
  html.window.sessionStorage.remove(_kVerifierKey);
  html.window.sessionStorage.remove(_kRedirectKey);
  html.window.history.replaceState(null, '', html.window.location.pathname ?? '/');

  if (verifier == null || verifier.isEmpty) {
    throw Exception(
        'OpenID PKCE verifier missing (sessionStorage was cleared mid-flow)');
  }

  print('[OpenID] got authorization code (len=${code.length}); '
      'server will exchange with redirect_uri=$redirectUri');
  return (
    idToken: null,
    accessToken: null,
    code: code,
    codeVerifier: verifier,
    redirectUri: redirectUri,
  );
}
