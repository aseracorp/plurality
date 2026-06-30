import 'package:openid_client/openid_client_io.dart';
import 'package:url_launcher/url_launcher.dart';

import 'openid_result.dart';

/// Runs the openid_client IO flow (desktop / Android / iOS): launches the
/// system browser at the provider's authorization endpoint and listens on a
/// loopback port for the redirect. This is the Authorization Code + PKCE flow,
/// which is why native works where web's implicit flow was rejected. Returns the
/// raw compact-serialized ID token plus the access token (needed server-side for
/// the userinfo fallback), which the caller exchanges for a Plurality JWT via
/// POST /auth/openid/exchange.
Future<OpenIDResult> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  final client = Client(issuerInfo, clientId);

  // Build the PKCE flow explicitly so we can point redirectUri at the loopback
  // IP literal (127.0.0.1) rather than `localhost`. Note: passing redirectUri to
  // the Authenticator() constructor would silently downgrade to the non-PKCE
  // authorizationCode flow, so we set it on the flow and use fromFlow instead.
  // RFC 8252 §7.3 recommends 127.0.0.1 over localhost for the loopback redirect.
  final flow = Flow.authorizationCodeWithPKCE(client)
    ..scopes.addAll(const ['openid', 'email', 'profile'])
    ..redirectUri = Uri.parse('http://127.0.0.1:4567/');

  final authenticator = Authenticator.fromFlow(
    flow,
    urlLancher: (url) async {
      await launchUrl(Uri.parse(url), mode: LaunchMode.externalApplication);
    },
  );

  final credential = await authenticator.authorize();
  try {
    await closeInAppWebView();
  } catch (_) {}

  final tokenResponse = await credential.getTokenResponse();
  final idToken = tokenResponse.idToken.toCompactSerialization();
  if (idToken.isEmpty) {
    throw Exception('Provider did not return an id_token');
  }
  return (
    idToken: idToken,
    accessToken: tokenResponse.accessToken,
    code: null,
    codeVerifier: null,
    redirectUri: null,
  );
}

/// Native uses a self-contained loopback flow that completes within a single
/// getOpenIDIdToken() call, so there's no pending redirect to pick up here.
Future<OpenIDResult?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async =>
    null;
