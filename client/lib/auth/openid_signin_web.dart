import 'dart:html' as html;

import 'package:openid_client/openid_client_browser.dart';

/// Flutter web's hash URL strategy clobbers the URL fragment on startup, which
/// would wipe an OIDC implicit-flow response (#id_token=...&access_token=...)
/// before openid_client reads window.location. A script in web/index.html
/// stashes that fragment in sessionStorage before Flutter boots; restore it
/// here so the openid_client browser flow can pick up the tokens. Idempotent:
/// the stash is consumed on first use.
void _restoreOAuthFragment() {
  final frag = html.window.sessionStorage['plurality_oauth_fragment'];
  if (frag == null || frag.isEmpty) return;
  html.window.sessionStorage.remove('plurality_oauth_fragment');
  final loc = html.window.location;
  html.window.history
      .replaceState(null, '', '${loc.pathname}${loc.search}#$frag');
}

/// Runs the openid_client browser flow. On the first call this redirects the
/// page to the provider's authorization endpoint and never returns. After the
/// provider redirects back, [completeOpenIDRedirect] (not this) picks up the
/// tokens during app startup.
Future<({String idToken, String? accessToken})> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
  print('[OpenID] getOpenIDIdToken: discovering issuer=$issuer clientId=$clientId');
  // Discover the issuer FIRST (this awaits a network round-trip). Only restore
  // the OAuth fragment immediately before constructing the Authenticator, which
  // reads window.location eagerly — any await between the two lets Flutter's
  // hash router run and wipe the fragment again.
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  print('[OpenID] issuer discovered: '
      'authEndpoint=${issuerInfo.metadata.authorizationEndpoint} '
      'responseTypes=${issuerInfo.metadata.responseTypesSupported}');
  final client = Client(issuerInfo, clientId);

  _restoreOAuthFragment();
  final authenticator = Authenticator(
    client,
    scopes: const ['openid', 'email', 'profile'],
  );

  final credential = await authenticator.credential;
  if (credential == null) {
    print('[OpenID] no credential yet — redirecting to provider authorize URL');
    authenticator.authorize();
    // The page is about to be replaced by the provider's auth screen.
    await Future.delayed(const Duration(seconds: 30));
    throw Exception('OpenID redirect did not complete');
  }
  print('[OpenID] credential present on initiate path — fetching token');
  final tokenResponse = await credential.getTokenResponse();
  final idToken = tokenResponse.idToken.toCompactSerialization();
  if (idToken.isEmpty) {
    throw Exception('Provider did not return an id_token');
  }
  return (idToken: idToken, accessToken: tokenResponse.accessToken);
}

/// Consumes the OIDC implicit-flow response captured by web/index.html before
/// Flutter booted (sessionStorage), falling back to the live URL fragment and
/// query string. Returns the raw `key=value&...` response string, or null when
/// there is no OAuth response in play (a normal startup). Removes the stash so
/// a reload doesn't replay it.
String? _consumeOAuthResponse() {
  var raw = html.window.sessionStorage['plurality_oauth_fragment'];
  html.window.sessionStorage.remove('plurality_oauth_fragment');

  String source = 'sessionStorage';
  if (raw == null || raw.isEmpty) {
    final h = html.window.location.hash ?? '';
    raw = h.startsWith('#') ? h.substring(1) : h;
    source = 'url#fragment';
  }
  // Some providers/proxies put the response in the query string instead.
  if (raw.isEmpty || !_looksLikeOAuthResponse(raw)) {
    final s = html.window.location.search ?? '';
    final q = s.startsWith('?') ? s.substring(1) : s;
    if (_looksLikeOAuthResponse(q)) {
      raw = q;
      source = 'url?query';
    }
  }

  print('[OpenID] consume: source=$source '
      'hash="${html.window.location.hash}" '
      'search="${html.window.location.search}" '
      'rawLen=${raw.length}');

  if (raw.isEmpty || !_looksLikeOAuthResponse(raw)) return null;
  return raw;
}

bool _looksLikeOAuthResponse(String s) =>
    RegExp(r'(^|&)(id_token|access_token|code|error)=').hasMatch(s);

/// Picks up an OpenID redirect that has already happened by parsing the tokens
/// straight out of the captured response, WITHOUT re-running the openid_client
/// browser flow. This avoids the fragile race where Flutter's hash router wipes
/// window.location before openid_client can read it. Returns the tokens, or null
/// when there is no redirect in progress. The server fully verifies the id_token
/// (signature/issuer/audience/expiry), so this client side only needs to relay
/// it and check the CSRF `state`.
Future<({String idToken, String? accessToken})?> completeOpenIDRedirect({
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

  final idToken = params['id_token'];
  if (idToken == null || idToken.isEmpty) {
    // We had an OAuth response but no id_token — e.g. the provider is set up for
    // the authorization-code flow (returns `code`) instead of implicit.
    if (params.containsKey('code')) {
      throw Exception(
          'OpenID provider returned an authorization code, not an id_token. '
          'This client uses the implicit flow — enable implicit/"id_token token" '
          'for this client, or it will never complete.');
    }
    throw Exception(
        'OpenID redirect contained no id_token (params: ${params.keys.join(", ")})');
  }

  // CSRF defense: the returned state must match what authorize() stored.
  final expectedState = html.window.localStorage['openid_client:state'];
  final returnedState = params['state'];
  if (expectedState != null &&
      expectedState.isNotEmpty &&
      returnedState != null &&
      returnedState != expectedState) {
    throw Exception('OpenID state mismatch (stale redirect or CSRF)');
  }
  html.window.localStorage.remove('openid_client:state');

  // Strip the OAuth response from the URL so a reload/logout doesn't replay it.
  html.window.history
      .replaceState(null, '', html.window.location.pathname ?? '/');

  print('[OpenID] parsed id_token (len=${idToken.length}), '
      'access_token=${params['access_token'] != null}');
  return (idToken: idToken, accessToken: params['access_token']);
}
