# Authentication and roles

doorstep authenticates two kinds of callers on its public listener:

- **People** sign in through one identity provider: a generic OpenID Connect issuer (Authentik, Authelia, Keycloak, Dex, Pocket ID, ...) or GitHub. Signing in creates a server-side session held in a cookie.
- **Programs** (CI jobs, scripts, the future CLI client) send an API token as `Authorization: Bearer dsp_...`.

Both resolve to one of three roles. The code lives in `internal/auth`; the decision is recorded in [ADR 0007](decisions/0007-authentication-and-roles.md).

## Roles

Roles are ordered, and each includes everything the lower ones may do:

| Role | May |
| --- | --- |
| `viewer` | read the UI and API: queue, locks, history, gate results, audit log |
| `deployer` | everything a viewer may, plus request a manual deploy or rollback, cancel a deployment, and take or release a freeze lock |
| `admin` | everything a deployer may, plus administer API tokens over HTTP |

Roles govern operating doorstep, never what ships. A merge to the deployed branch is still the approval for a deploy ([ADR 0001](decisions/0001-go-and-core-design.md)); no role is needed for a webhook-triggered deploy, and no role can make doorstep deploy a commit that was not merged.

The UI and API beads attach the required role to each route; this bead provides the mechanism and no protected routes yet. Every route that needs identity opts in, and there is no switch that disables authentication.

## API tokens

Tokens are created, listed and revoked with the CLI, which talks to the database directly. Whoever holds the database URL already controls doorstep, so this needs no bootstrap password or first-run wizard, and it works through `kubectl exec` into the running image.

```sh
doorstep token create --name ci-blog --role deployer --expires-in 720h
doorstep token list
doorstep token revoke ci-blog
```

- `create` prints the token once on stdout and the details on stderr, so `TOKEN=$(doorstep token create ...)` captures only the token. It cannot be shown again; create a new one if it is lost.
- `--name` is required and unique for the life of the database: lowercase letters, digits, `.`, `_` and `-`, starting with a letter or digit, at most 63 characters. Revoked tokens keep their names, so an audit row that names a token always means the same token.
- `--role` is required: `viewer`, `deployer` or `admin`.
- `--expires-in` is a Go duration (`720h`); `0`, the default, never expires.
- `list` prints every token with its role, creation, expiry and last use times and its state (`active`, `expired` or `revoked`). `last used` is updated at most once a minute.
- `revoke` takes effect on the next request.

The commands accept the same `--config`, `--database-url-file`, `--log-level` and `--log-format` flags and environment variables as [`doorstep migrate`](configuration.md#doorstep-migrate). They do not apply migrations, and fail with `database schema is not up to date; run doorstep migrate first` when the schema is behind. Each `create` and `revoke` writes an audit row. Inside a container the operating system user is a fixed account such as `nonroot`, which says nothing about who acted, so name yourself with `--actor NAME` or `DOORSTEP_ACTOR=NAME` (the flag wins): the actor is then `cli:NAME`, and the operating system user is kept as `origin.os_user` in the row's details. Without either, the actor is `cli:<operating system user>`.

```sh
kubectl exec deploy/doorstep -- doorstep token revoke ci-blog --actor alice
```

A token is `dsp_` followed by 43 base64url characters (32 random bytes). The fixed prefix lets secret scanners recognise leaked tokens. Only the SHA-256 hash of the token is stored; a database dump does not reveal usable tokens.

## Sign-in

Sign-in needs three things in the configuration file and the runtime settings:

1. `server.public_url` (`--public-url`, `DOORSTEP_PUBLIC_URL`): the URL people use to reach doorstep, such as `https://deploy.example.com`. It must be `https`, or `http` only for a loopback host (`localhost`, `127.0.0.1`, `::1`) during development. It has no path: doorstep is served from the root of its host. The redirect (callback) URI registered with the provider is `<public_url>/auth/callback`.
2. Exactly one provider block: `auth.oidc` or `auth.github`. Configuring both is an error.
3. Role mapping in `auth.roles` and, optionally, `auth.default_role`.

With no provider configured, the `/auth/*` routes are not mounted and only API tokens authenticate.

The full key reference is in [configuration.md](configuration.md#auth). `doorstep config validate` checks all of it, including that `server.public_url` is set when a provider is.

### OpenID Connect

```yaml
server:
  public_url: https://deploy.example.com
auth:
  oidc:
    issuer_url: https://auth.example.com/application/o/doorstep/
    client_id: doorstep
    client_secret_file: /etc/doorstep/secrets/oidc-client-secret
  roles:
    admin:
      groups: [homelab-admins]
    deployer:
      groups: [developers]
      users: [alice@example.com]
```

Register doorstep at the identity provider as a confidential client (it has a client secret) using the authorization code flow, with redirect URI `https://deploy.example.com/auth/callback`. doorstep uses PKCE (S256), `state` and `nonce` on every sign-in.

- Requested scopes default to `openid profile email groups`. Set `auth.oidc.scopes` when the provider names its groups scope differently, and keep `openid`.
- Groups are read from the `groups` claim (`auth.oidc.groups_claim`), which may be a list of strings or a single string. The user's actor ID is `oidc:<sub>`, so audit rows and future lock owners stay tied to one account even when two accounts share a username or a username changes. `preferred_username` (`auth.oidc.username_claim`), falling back to the subject, is only the label shown next to it in logs, `doorstep session list` and the audit row's actor name; the display name comes from `name`.
- Claims come from the verified ID token (signature, issuer, audience = client ID, expiry and nonce), plus any claims the ID token lacks from the UserInfo endpoint after checking that its `sub` matches. Providers that only return groups from UserInfo, as Authelia does by default, therefore work.
- The issuer's discovery document is fetched on first use and cached. Concurrent sign-ins share one fetch, each waiting only as long as its own request lasts, and a failed fetch is remembered for 5 seconds, so while the issuer is down sign-ins fail fast instead of queueing behind 10-second timeouts. `serve` checks it once at startup and logs `identity provider unreachable` as a warning if it fails, but still starts: an outage of the identity provider only stops sign-in, never deploys or token access.
- Signing out ends the doorstep session only; it does not sign out of the identity provider.

Provider notes:

- **Authentik**: create an OAuth2/OpenID provider (confidential), and an application using it. The issuer is `https://<host>/application/o/<application slug>/`. Authentik puts groups in the `groups` claim through its default `profile` scope mapping and has no `groups` scope, so set `scopes: [openid, profile, email]`.
- **Authelia**: add a client with `token_endpoint_auth_method: client_secret_basic` (or `client_secret_post`), `require_pkce: true`, `pkce_challenge_method: S256` and the scopes `openid profile email groups`. The issuer is Authelia's root URL.
- **Keycloak**: create a confidential OpenID Connect client with standard flow enabled. Keycloak does not include groups by default: add a *Group Membership* mapper with token claim name `groups` and *Full group path* turned off (or match the full paths such as `/admins` in `auth.roles`). The issuer is `https://<host>/realms/<realm>`.
- **Dex** and **Pocket ID** work with the defaults.

### GitHub

```yaml
server:
  public_url: https://deploy.example.com
auth:
  github:
    client_id: Ov23liExample
    client_secret_env: DOORSTEP_GITHUB_CLIENT_SECRET
  roles:
    admin:
      users: ["id:583231"]
    deployer:
      groups: [example-org/platform]
    viewer:
      groups: [example-org]
```

The client can be either of:

- **An OAuth App** (Settings, Developer settings, OAuth Apps) with authorization callback URL `https://deploy.example.com/auth/callback`. doorstep requests the `read:org` scope, which lets it read organization and team memberships, including private ones. This is the simplest choice when rules use teams.
- **The GitHub App doorstep already uses** ([github-app.md](github-app.md)): add the callback URL under *Callback URL*, generate a client secret, and use the App's Client ID and secret here. GitHub Apps ignore scopes; the user token can only see what the App's permissions and installations allow. Organization rules need the App installed on that organization, and team rules additionally need the *Members* organization permission (read). The App then only sees organizations and teams where it is installed. If granting *Members: read* to the deploying App is not acceptable, use an OAuth App for sign-in instead.

During sign-in doorstep uses the user's token once, in memory, to read `GET /user`, and only when some rule needs them, `GET /user/memberships/orgs?state=active` (for organization rules) and `GET /user/teams` (for team rules), each capped at 10 pages of 100. The token is then discarded and never stored. Requests go only to the host of `github.api_url`; for GitHub Enterprise Server (`https://<host>/api/v3`) the OAuth endpoints are `https://<host>/login/oauth/authorize` and `https://<host>/login/oauth/access_token`.

GitHub users are identified by their numeric user ID, never their login: a login can be renamed, and the freed login can then be registered by someone else. `users` entries are therefore `id:` followed by the ID (`id:583231`), and logins are rejected by `doorstep config validate`. Look an ID up with `gh api users/<login> --jq .id` or `https://api.github.com/users/<login>`. The actor ID of a GitHub user is `github:<id>`, with the login shown as the actor's name.

### Role mapping

`auth.roles` maps each role to `groups` and `users`. On sign-in, doorstep checks `admin`, then `deployer`, then `viewer`, and grants the first role with a matching entry, so the highest match wins. If nothing matches, it grants `auth.default_role`; when that is empty (the default), sign-in is refused with `403` and a page saying the account is not authorised. Authenticating with the identity provider is never enough on its own.

| Provider | `groups` entries match | `users` entries match |
| --- | --- | --- |
| `oidc` | values of the groups claim, exactly (case-sensitive) | the `sub` claim exactly, or the `email` claim case-insensitively when `email_verified` is true |
| `github` | `org` (active organization membership) or `org/team-slug`, case-insensitively | `id:<numeric user ID>`, matched against the ID GitHub returns for the signed-in user |

The role is decided at sign-in and stored in the session. Changing group membership or `auth.roles` takes effect when the user next signs in, so `auth.session_ttl` bounds how long a removed user keeps access. To cut access immediately, revoke the user's sessions; see [Ending sessions](#ending-sessions).

## Sessions and cookies

- A successful sign-in creates a row in `auth_sessions` holding the SHA-256 hash of a random 32-byte session token, the identity (provider, subject, username, name, email) and the role. The cookie holds the token itself. Nothing in the database can be turned back into a valid cookie.
- Sessions last `auth.session_ttl` (default `12h`, between `5m` and `720h`) from sign-in. They are not extended by activity, and there are no refresh tokens. No ID token or provider access token is stored.
- `POST /auth/logout` deletes the session row and the cookie. Expired sessions and abandoned sign-ins are deleted when a new sign-in starts, at most once a minute per replica.
- At most 10,000 sign-ins may be in progress (started in the last 10 minutes and not completed). Beyond that `GET /auth/login` answers `503` with `Retry-After` and logs `sign-in refused`, so anonymous clients cannot grow `auth_logins` without bound.
- Sessions live in Postgres, so any replica can serve any request and there is no signing key to manage.

With an `https` public URL the cookies are `__Host-doorstep-session` and `__Host-doorstep-login`, `Secure`, `HttpOnly`, `SameSite=Lax` and `Path=/`. The `__Host-` prefix stops sibling subdomains from setting them. With a loopback `http` public URL they are named `doorstep-session` and `doorstep-login` without `Secure`. `SameSite=Lax` (not `Strict`) is needed because the identity provider redirects back to doorstep with a cross-site navigation.

### Ending sessions

```sh
doorstep session list
doorstep session revoke --user alice
doorstep session revoke --user github:583231
doorstep session revoke --all
```

- `list` prints every active session with its actor ID, username, role, creation and expiry time.
- `revoke --user` ends every session whose username matches case-insensitively, whose subject matches, or whose actor ID (`provider:subject`) matches. Prefer the actor ID, which cannot be shared or renamed. `revoke --all` signs everyone out.
- Revoked sessions stop working on the next request. The user can sign in again and gets the role the current configuration grants; to keep someone out, also remove them from `auth.roles` or their groups at the provider.
- Each `revoke` writes a `session.revoke` audit row with the selector and the number of sessions ended, even when none matched.

The commands take the same database and `--actor` flags as `doorstep token`.

### The sign-in flow

1. `GET /auth/login?return_to=/path` creates a row in `auth_logins` keyed by the hash of a random `state`, holding the nonce, PKCE verifier and return path, valid for 10 minutes. It sets the short-lived login cookie to the raw state and redirects to the provider.
2. The provider redirects to `GET /auth/callback?code=...&state=...`. doorstep checks that the `state` parameter equals the login cookie, consumes the `auth_logins` row (each state works once), exchanges the code with the PKCE verifier, verifies the identity, maps the role, creates the session, writes an `auth.login` audit row and redirects to the return path.

`return_to` must be a path on doorstep: it must start with exactly one `/` and contain no backslash or control characters, and it must not parse as having a scheme or host. Anything else becomes `/`. The return path never travels through the identity provider.

Errors on the callback show a short generic message (`Sign-in could not be verified`, `Sign-in expired`, `Sign-in failed`) with `Cache-Control: no-store`; the cause is logged.

## CSRF protection

Requests authenticated by the session cookie with an unsafe method (anything but `GET`, `HEAD`, `OPTIONS` and `TRACE`) must pass two checks, which the authentication middleware enforces before any handler runs:

1. The Fetch Metadata and `Origin` check of Go's `http.CrossOriginProtection`, with the public URL's origin trusted. Cross-origin browser requests are rejected.
2. A per-session CSRF token, sent in the `X-CSRF-Token` header or as the `csrf_token` form field. It is derived from the session token with HMAC-SHA256, so it needs no storage and changes with every session.

A request that fails either check gets `403 {"error":"forbidden"}`. Requests with a bearer token are exempt, since browsers never attach one by themselves.

UI authors get the token for the current request from `auth.CSRFToken(r.Context())`:

```html
<form method="post" action="/auth/logout">
  <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
  <button>Sign out</button>
</form>
```

For htmx, set the header once, for example `<body hx-headers='{"X-CSRF-Token": "{{.CSRFToken}}"}'>`.

## Using authentication in handlers

`serve` wraps every route mounted on the public listener in `Authenticator.Middleware`, which resolves the caller and stores a `Principal` in the request context. The built-in `/healthz` and `/readyz`, and `/metrics` when it shares the main listener, are not wrapped, so a probe or scraper that sends a stale `Authorization` header still gets an answer. The metrics listener is never authenticated.

1. `Authorization: Bearer <token>`: a valid token yields a principal with `Method` `token`. An invalid, expired or revoked token is answered with `401` straight away; it never falls back to the cookie.
2. Otherwise the session cookie: a valid session yields a principal with `Method` `session`, subject to the CSRF checks above. An unknown or expired session cookie is cleared and the request continues anonymously.
3. Otherwise the request continues anonymously, so the signature-verified GitHub webhook needs no special case.

Handlers that need identity opt in by wrapping themselves:

```go
deployer := svc.Authenticator().RequireRole(core.RoleDeployer)
routes["POST /api/v1/apps/{app}/deploy"] = deployer(http.HandlerFunc(api.deploy))

viewer := svc.Authenticator().RequireRoleOrLogin(core.RoleViewer)
routes["GET /"] = viewer(http.HandlerFunc(web.index))
```

- `RequireRole(role)` answers an anonymous request with `401 {"error":"unauthenticated"}` and `WWW-Authenticate: Bearer realm="doorstep"`, and a principal whose role is too low with `403 {"error":"forbidden"}`. Bodies never say why authentication failed.
- `RequireRoleOrLogin(role)` redirects anonymous `GET` and `HEAD` requests to `/auth/login?return_to=<path>` when a sign-in provider is configured, and otherwise behaves like `RequireRole`. Use it for UI pages.
- `auth.PrincipalFrom(ctx)` returns the principal. `Principal.Actor.String()` is the canonical actor string, such as `user:oidc:3f2a...`, `user:github:583231`, `token:ci-blog`, `cli:alice` or `system`, to store in `deployments.requested_by`, `locks.owner` and `deployment_events.actor`.

Failed authentications and denied authorizations are logged at warn with the method, path and actor, never with token material.

## Audit log

The `audit_log` table records who asked doorstep to do something. It complements `deployment_events`, which records what happened to a deployment, and also covers actions that have no deployment: token administration, sign-in and sign-out, and locks.

| Action | Written by |
| --- | --- |
| `auth.login`, `auth.logout` | the sign-in handler |
| `token.create`, `token.revoke` | `doorstep token`, and later the token API |
| `session.revoke` | `doorstep session revoke` |
| `rollback.request` | `POST /api/v1/apps/{app}/rollback` ([rollback.md](rollback.md)), in the same transaction as the rollback deployment and its block |
| `block.create`, `block.clear` | `POST /api/v1/apps/{app}/blocks` (only when a new block is created) and `DELETE /api/v1/apps/{app}/blocks/{ref}` |
| `deploy.request`, `deploy.cancel` | the API and UI, when they land |
| `lock.acquire`, `lock.release` | the locks feature, when it lands |

Rollback and block requests need the `deployer` role and record the caller's actor and role. The `deploy` and `lock` actions are reserved constants only: nothing records them yet. The features that add those endpoints own both halves of the requirement: wrapping each route in `RequireRole`, and recording the audit row in the same transaction as the action.

Each row holds the time, the actor (kind, ID and display name), the actor's role when known, the action, the target app and environment and the deployment ID when they apply, and a JSON object with details. Actions are dot-separated lowercase words; new features add constants to `core` without a schema change.

The table is append-only: a trigger rejects every `UPDATE`, `DELETE` and `TRUNCATE`, and rows have no foreign keys, so they outlive deleted apps and deployments. Denied requests are not written to it, so unauthenticated traffic cannot grow it.

Code records an entry with `(*store.Store).RecordAudit(ctx, core.AuditEntry{...})`. Callers declare a one-method port for it, and should call it on the transaction-bound store inside the same `InTx` as the action it describes, so an action and its audit row commit or roll back together:

```go
type AuditRecorder interface {
	RecordAudit(ctx context.Context, e core.AuditEntry) error
}
```

`(*store.Store).ListAudit` reads entries newest first, filtered by target, actor or action, with keyset pagination. Until the API exposes it, query the table directly:

```sql
SELECT occurred_at, actor_kind || ':' || actor_id AS actor, actor_role, action, app, environment, deployment_id, detail
FROM audit_log
ORDER BY id DESC
LIMIT 50;
```

There is no retention yet; rows are kept forever.
