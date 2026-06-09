import 'package:openid_client/openid_client_io.dart';
import 'package:url_launcher/url_launcher.dart';

/// Runs the openid_client IO flow (desktop / Android / iOS): launches the
/// system browser at the provider's authorization endpoint and listens on a
/// loopback port for the redirect. Returns the raw compact-serialized ID token
/// plus the access token (needed server-side for the userinfo fallback), which
/// the caller exchanges for a Plurality JWT via POST /auth/openid/exchange.
Future<({String idToken, String? accessToken})> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) async {
  final issuerInfo = await Issuer.discover(Uri.parse(issuer));
  final client = Client(issuerInfo, clientId);

  final authenticator = Authenticator(
    client,
    scopes: const ['openid', 'email', 'profile'],
    port: 4567,
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
  return (idToken: idToken, accessToken: tokenResponse.accessToken);
}

/// Native uses a self-contained loopback flow that completes within a single
/// getOpenIDIdToken() call, so there's no pending redirect to pick up here.
Future<({String idToken, String? accessToken})?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async =>
    null;
