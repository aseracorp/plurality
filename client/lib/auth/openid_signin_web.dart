import 'package:openid_client/openid_client_browser.dart';

/// Runs the openid_client browser flow. On the first call this redirects the
/// page to the provider's authorization endpoint and never returns. After the
/// provider redirects back, the call returns the stored credential's ID token.
///
/// Callers should invoke this on app startup to pick up a redirect that has
/// already happened, and again from the login button to start a fresh flow.
Future<String> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
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
  return idToken;
}

/// Picks up an OpenID redirect that has already happened (token present in the
/// page URL fragment) WITHOUT starting a new flow. Returns the ID token if a
/// credential is waiting, or null otherwise. Call this on app startup.
Future<String?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async {
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
  return idToken.isEmpty ? null : idToken;
}
