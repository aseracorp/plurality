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
/// provider redirects back, the call returns the stored credential's ID token.
///
/// Callers should invoke this on app startup to pick up a redirect that has
/// already happened, and again from the login button to start a fresh flow.
Future<({String idToken, String? accessToken})> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
  _restoreOAuthFragment();
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  final client = Client(issuerInfo, clientId);

  final authenticator = Authenticator(
    client,
    scopes: const ['openid', 'email', 'profile'],
  );

  final credential = await authenticator.credential;
  if (credential == null) {
    authenticator.authorize();
    // The page is about to be replaced by the provider's auth screen.
    await Future.delayed(const Duration(seconds: 30));
    throw Exception('OpenID redirect did not complete');
  }
  final tokenResponse = await credential.getTokenResponse();
  final idToken = tokenResponse.idToken.toCompactSerialization();
  if (idToken.isEmpty) {
    throw Exception('Provider did not return an id_token');
  }
  return (idToken: idToken, accessToken: tokenResponse.accessToken);
}

/// Picks up an OpenID redirect that has already happened (token present in the
/// page URL fragment) WITHOUT starting a new flow. Returns the tokens if a
/// credential is waiting, or null otherwise. Call this on app startup.
Future<({String idToken, String? accessToken})?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async {
  _restoreOAuthFragment();
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  final client = Client(issuerInfo, clientId);
  final authenticator = Authenticator(
    client,
    scopes: const ['openid', 'email', 'profile'],
  );
  final credential = await authenticator.credential;
  if (credential == null) {
    return null;
  }
  final tokenResponse = await credential.getTokenResponse();
  final idToken = tokenResponse.idToken.toCompactSerialization();
  if (idToken.isEmpty) {
    return null;
  }
  // Strip the OAuth response from the URL fragment so a reload or logout
  // doesn't replay the same credential and re-trigger a login.
  html.window.history
      .replaceState(null, '', html.window.location.pathname ?? '/');
  return (idToken: idToken, accessToken: tokenResponse.accessToken);
}
