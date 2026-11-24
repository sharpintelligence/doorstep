# 0007: Authentication and roles

- Status: Accepted
- Date: 2026-11-24

## Context

doorstep is about to gain an HTTP API and a web UI that can request deploys and rollbacks, cancel deployments and freeze targets. Those actions need to know who is asking, the history needs to say who did what, and some people should be able to look without being able to act. [ADR 0001](0001-go-and-core-design.md) says the merge is the approval and that doorstep has no role model of its own for deciding what ships. Any role model has to fit that.

The audience is homelab operators and small teams. Many already run an OpenID Connect provider (Authentik, Authelia, Keycloak, Dex, Pocket ID); all of them already have GitHub, because doorstep requires a GitHub App. doorstep may run several replicas behind one ingress.

## Decision

1. **Roles gate operating doorstep, not what ships.** Three ordered roles, `viewer < deployer < admin` (`core.Role`), checked with one comparison (`Role.Allows`). A viewer reads; a deployer may also request a manual deploy or rollback, cancel a deployment and take or release a freeze lock; an admin may also administer API tokens. A webhook-triggered deploy of a merged commit needs no role, and no role can make doorstep deploy an unmerged commit.
2. **One sign-in provider per instance.** Either generic OpenID Connect or GitHub, configured in the `auth` section of the configuration file, behind one `auth.IdentityProvider` interface. Configuring both is an error. With neither, only API tokens authenticate. There is no switch to turn authentication off.
3. **Deny by default.** Roles are mapped from groups and users in `auth.roles`, highest match wins, and `auth.default_role` is empty unless set, so signing in with the identity provider never grants access on its own.
4. **API tokens are random, hashed and administered from the CLI.** A token is `dsp_` plus 32 random bytes in base64url; only its SHA-256 is stored. `doorstep token create|list|revoke` uses the database directly, so there is no bootstrap credential.
5. **Server-side sessions in Postgres.** The cookie holds a random token, the database its hash and an identity snapshot with the role. Sessions have an absolute lifetime (`auth.session_ttl`, default 12 hours) and no refresh. The in-flight sign-in (state, nonce, PKCE verifier, return path) is also a single-use database row.
6. **Security attributes derive from one setting.** `server.public_url` must be `https` (or `http` on loopback); it sets the cookie names (`__Host-` prefix), the `Secure` flag, the redirect URI and the trusted origin for CSRF checks.
7. **CSRF is enforced by the authenticator.** Session-authenticated unsafe requests must pass Go's `http.CrossOriginProtection` and present a per-session HMAC token. Bearer-token requests are exempt.
8. **Authentication is one middleware on the public listener**, resolving a `Principal` into the request context; anonymous requests pass through, and handlers opt in to a required role with `RequireRole` or `RequireRoleOrLogin`.
9. **An append-only audit log in Postgres.** `audit_log` records who requested what (actor, role, action, target, deployment, details). Writes go through `(*store.Store).RecordAudit`, so a caller writes the audit row in the same transaction as the action. A trigger rejects updates, deletes and truncation. Actions are validated by format, not by an enumerated list.
10. **No users table.** The identity provider is the user directory; sessions and audit rows carry the identity they need.

## Options considered

### Roles: ordered roles (chosen), a permission matrix, or no roles

A permission matrix (per action, per app) is more expressive but needs its own configuration, UI and tests, and would start to look like the approval system ADR 0001 rules out. No roles at all would let anyone who can sign in cancel deployments and freeze production. Three ordered roles cover "look", "operate" and "administer" with a single comparison.

### Providers: OIDC only, GitHub only, both at once, or one of either (chosen)

OIDC only excludes operators without an identity provider; GitHub only ignores the homelab identity providers people already run. Allowing both at once needs a provider chooser, account linking and a way to tell `admins` from one provider apart from `admins` from the other. One provider per instance keeps a single group namespace.

### Sessions: signed stateless cookies, or server-side sessions (chosen)

Signed cookies need a signing key that has to be generated, mounted, rotated and shared across replicas, and they cannot be revoked before they expire. A database row costs one indexed lookup per request, which is negligible at doorstep's scale, and makes sign-out and revocation immediate.

### Token administration: HTTP with a bootstrap admin, or CLI with database access (chosen)

An HTTP-only path needs a first credential (a default password, a setup wizard or a token printed in the logs), each of which is a well-known weakness. Anyone with the database URL already controls doorstep, so the CLI adds no new trust. The `auth.Tokens` service is exported so admin-only HTTP endpoints can be added later.

### Audit recorder: an `audit` package, inside `auth`, or a store method (chosen)

Adapters may not import each other, so locks or the engine calling into `auth` to record an action would break the dependency rules, and a separate package would only wrap the store. A store method satisfies a one-method port declared by each consumer and works on a transaction-bound store.

## Consequences

- `internal/auth` exists and imports `core`, `store` and `github` (for GitHub sign-in); the `auth -> github` edge is a documented exception to the adapter rules.
- The API and UI must wrap every handler that needs identity with `RequireRole` or `RequireRoleOrLogin`; anything they forget is anonymous rather than broken, so their tests must check the role of every route.
- `core.Actor.String()` (`user:oidc:<sub>`, `user:github:<id>`, `token:ci`, `cli:alice`, `system`) is the value for the existing `requested_by`, `owner` and `actor` columns, so attribution needs no schema change. A user's actor ID is `provider:subject`, never a username or login, because those can be changed or reused and lock ownership will compare these strings; the username is only the actor's display name. GitHub role bindings name users by numeric ID (`id:583231`) for the same reason.
- A role change or a removal from a group takes effect at the next sign-in; the session lifetime bounds how stale a role can be. `doorstep session revoke` ends sessions early and records a `session.revoke` audit row.
- GitHub team rules with GitHub App credentials need the App's *Members: read* permission; an OAuth App avoids widening the deploying App.
- The `deploy`, `rollback` and `lock` audit actions exist only as constants until their features land; those features must record the audit row inside the action's transaction and wrap their routes in `RequireRole`.
- Audit rows are kept forever until a retention policy is added as a deliberate migration.
- An outage of the identity provider stops sign-in but not deploys or token access.
