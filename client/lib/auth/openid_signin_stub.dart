// Stub implementation. Replaced at compile time via conditional imports in
// `openid_signin.dart`.
Future<String> getOpenIDIdToken({
  required String issuer,
  required String clientId,
}) {
  throw UnsupportedError('OpenID sign-in is not available on this platform');
}

/// No redirect-based flow on unsupported platforms.
Future<String?> completeOpenIDRedirect({
  required String issuer,
  required String clientId,
}) async =>
    null;
