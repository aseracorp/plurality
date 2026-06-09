# OpenID / OIDC Login

Plurality supports logging in via an external OpenID Connect provider
(Authentik, Keycloak, Auth0, Cosmos, Google, etc.) in addition to local
accounts.

## How it works (no secrets)

The Flutter client (both web and native) drives the OAuth flow itself using
**PKCE**, and the server only **verifies the resulting ID token**. This means:

- **No client secret is needed** anywhere.
- The OAuth client must be registered as a **public client** in your provider
  (token-endpoint auth method = `none`).
- There is no server-side redirect/callback route — the client handles the
  redirect and posts the ID token to `/auth/openid/exchange`.

## Enabling

OpenID is enabled automatically as soon as `OPENID_ISSUER` is set. All
configuration is done through environment variables (env always wins over
`data/config.json`).

| Env var | Required | Purpose |
|---|---|---|
| `OPENID_ISSUER` | ✅ | Provider issuer URL — setting this **enables** OpenID. e.g. `https://auth.example.com` |
| `OPENID_NAME` | optional | Human-friendly provider label shown in the UI (e.g. `Authentik`). Defaults to `OpenID`. |
| `OPENID_CLIENT_ID` | ✅ | OAuth **public** client ID registered with the provider. |
| `OPENID_ALLOWLIST` | optional | Comma-separated list of allowed emails / usernames / nicknames. Empty = anyone with a valid login is allowed. |
| `JWT_SECRET` | optional | Override the auto-generated JWT signing secret. |

### Example

```sh
export OPENID_ISSUER="https://auth.example.com"
export OPENID_NAME="Authentik"
export OPENID_CLIENT_ID="plurality"
# optional — omit entirely to allow everyone:
export OPENID_ALLOWLIST="azukaar@gmail.com, alice, *@mycompany.com"
```

## The whitelist (`OPENID_ALLOWLIST`)

The allowlist is **optional**. If it contains no usable entries, every
authenticated OpenID user is allowed to log in. When it does contain entries,
a user is allowed only if their **email**, **username**, or **nickname**
matches one of them.

Entry formats (all case-insensitive):

| Entry | Matches |
|---|---|
| `*` | any authenticated user |
| `*@example.com` | any user whose **email** ends in `@example.com` |
| `azukaar@gmail.com` | that exact email |
| `alice` | a user whose username/nickname is `alice` |

Matching is done against the email and the name claims returned by the
provider (`preferred_username`, `username`, `nickname`, `name`).

## Provider configuration

On the provider side, register an **application / public client** and:

- **Client type:** public (token-endpoint auth method `none`, PKCE). No secret.
- **Redirect URIs** — register both, exactly (trailing slash matters):
  - `http://localhost:4567/` — for the native app (desktop/mobile loopback).
  - `https://<your-plurality-host>/` — for the web app (it redirects back to
    its own origin root).
- **Grants / response types:** allow authorization code + PKCE (native) and,
  for web, the implicit response type `token id_token`.
- **Scopes:** ensure `openid`, `email`, and `profile` are granted. The name
  shown in Plurality is taken from the first available of `preferred_username`,
  `username`, `nickname`, `name`, falling back to the email.

## Endpoints

| Route | Method | Purpose |
|---|---|---|
| `/auth/methods` | GET | Reports available login methods. When OpenID is enabled, also returns `openid_name`, `openid_issuer`, and `openid_client_id`. |
| `/auth/openid/exchange` | POST | The client POSTs `{"id_token": "..."}` and receives `{"token", "username"}`. The server verifies the token signature, applies the allowlist, and issues a Plurality JWT. |

## Notes

- OpenID-only users do not have a local password; the change-password
  endpoint returns an error for them.
- The local username derived from OpenID is sanitized for safe use as a
  directory name (slashes and `..` are stripped).
