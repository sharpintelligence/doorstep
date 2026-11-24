# 0018: Server-rendered web UI

- Status: Accepted
- Date: 2026-11-24

## Context

doorstep needs a UI for people who are not at a terminal: what each app runs, what is queued, who holds a lock, the history with lineage and gate results, and buttons for deploy, cancel, rollback, unblock, lock and token administration. The architecture already chose `html/template` and htmx vendored through `embed`, with no CDN and no JavaScript build step. [ADR 0007](0007-authentication-and-roles.md) makes the authenticator the single enforcement point for sessions, roles and CSRF, and requires every UI route to be wrapped and tested per route. The pages show text that doorstep does not control: gate messages and evidence from external systems, commit data, lock reasons. The audience is a homelab or a small team: a handful of apps and viewers, often on a home network, sometimes with JavaScript blocked.

## Decision

1. **A package `internal/web` contributes routes the same way `auth` does.** `web.New(Options)` returns a handler whose `Routes()` map `serve` copies into the application routes, so the UI sits behind the authentication middleware, request logging and panic recovery like the API. It reads through small ports satisfied by existing types (`*store.Store`, `*queue.Queue`, `*locks.Service`, `*rollback.Service`, `*auth.Tokens`, the deploy service of [ADR 0019](0019-manual-deploys-and-cancellation.md)); there is no new configuration, and the only new SQL is the bulk reads that let the overview show every app in a fixed number of queries (`Store.CurrentDeployments`, `Store.LatestDeployments`, and `QueueEntries` for every app). The UI is always mounted.
2. **One route table drives registration and roles.** Each page and action is a `(pattern, role, handler)` row. Pages need `viewer`; actions need `deployer`; tokens need `admin`; static files and the HTML 404 need none. Every role route is wrapped in `RequireRoleOrLogin(viewer)` and then in the UI's own check, which answers a signed-in user with too low a role with an HTML `403` instead of auth's JSON. A thin outer check handles anonymous htmx requests (`401` with `HX-Redirect` to sign-in, instead of a redirect the XHR would follow to the identity provider) and installations without a sign-in provider (an HTML `401` explaining why).
3. **CSRF stays with the authenticator.** Every POST form carries the session's token from `auth.CSRFToken`; the body carries it for htmx in `hx-headers`. The UI has no CSRF mechanism of its own.
4. **HTML forms do the work; htmx only polls.** Actions are POST forms answered with `303` (post/redirect/get); errors re-render the page with the message at the proper status. The changing part of a page is a `#live` region that htmx re-fetches from the same URL every 5 seconds and swaps with `hx-select`. There are no fragment endpoints and no `hx-boost`. Destructive actions sit in `<details>` (with a reason field where the action records one) rather than JavaScript confirmations. Actions that depend on the state shown sit inside `#live`, and `app.js` skips a poll while a disclosure in it is open or a field in it is in use, so rows never move under an open confirm button and typing is never lost. Success messages are fixed codes in the query string mapped to constant text; the one secret, a newly created API token, is rendered once in the POST response with `no-store`.
5. **A strict Content Security Policy.** `default-src 'none'`, scripts and styles only from `'self'`, `form-action 'self'`, `frame-ancestors 'none'`, no inline script or style anywhere, and htmx configured without eval, script tags or injected styles. Together with `html/template` escaping this is the defence against hostile text in gate results. Evidence URLs become links only when they are absolute `http` or `https` URLs.
6. **htmx 2.0 is vendored with a checksum.** The minified file, its licence and a `VERSIONS` record with its sha256 are committed; a unit test fails when they disagree. Static files are embedded and served with a content hash in the URL and immutable caching.

## Options considered

### A single-page app with a JSON API

It would reuse the API, but needs a JavaScript toolchain, a bundler and a second set of dependencies to keep patched, and it moves escaping and authorization decisions into the browser. Rejected for a tool whose pages are tables and forms.

### htmx fragments and `hx-boost` for every interaction

Fragment endpoints double the routes (each needing its own role and tests) and let a fragment and its full page drift apart; `hx-boost` hides form posts behind XHR, which complicates error statuses and the no-JavaScript path. Polling the full page and selecting one region costs one render per poll, which is trivial at this scale.

### A flash cookie for success messages

It would need its own cookie attributes, signing or storage, duplicating what `auth` already gets right, and would let user-controlled text reach the page. A fixed code cannot carry anything but the messages the server defines.

### Loading htmx from a CDN

Breaks offline installs, adds a third party to every page load and needs `script-src` to name it. Vendoring with a checksum keeps what runs reviewable.

### Reusing auth's JSON `403` and redirects as they are

A JSON body is the wrong answer for a browser page, and an htmx poll that follows a redirect to an identity provider fails silently on the cross-origin hop, leaving a stale page that looks live.

## Consequences

- The UI works with JavaScript disabled; only live refresh and local time tooltips are lost.
- Every new page or action is one row in the route table and must state its role; the table test fails until the expected role is added.
- A poll renders a full page, a few queries per app on the overview. That bounds the UI to the homelab scale it is built for.
- The UI needs a sign-in provider; API tokens deliberately do not open it.
- htmx updates are a manual, reviewed step ([ui.md](../ui.md#vendored-htmx)).
