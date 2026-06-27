/// The result of an OpenID sign-in step, sent to the server's
/// POST /auth/openid/exchange to obtain a Plurality JWT.
///
/// The two platforms produce different shapes because they complete the OAuth
/// flow differently:
///   - Native (io) runs the Authorization Code + PKCE flow itself (loopback
///     redirect) and exchanges the code locally, so it fills [idToken] /
///     [accessToken].
///   - Web cannot exchange the code in the browser reliably — the provider's
///     token endpoint usually isn't CORS-enabled — so it returns the
///     authorization [code] / [codeVerifier] / [redirectUri] and lets the
///     server perform the PKCE token exchange. (Web cannot use the OAuth
///     implicit flow because providers like Ory forbid the implicit grant.)
typedef OpenIDResult = ({
  String? idToken,
  String? accessToken,
  String? code,
  String? codeVerifier,
  String? redirectUri,
});
