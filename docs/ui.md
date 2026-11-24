# Web UI

`doorstep serve` serves a small server-rendered web UI on the main listener, next to the API. It shows what each app runs, what is queued, who holds which lock, the full deploy history with lineage and gate results, and the audit log, and it lets deployers deploy, cancel, roll back, block, unblock and lock. It is built by `internal/web` with `html/template` and a vendored htmx that only refreshes pages; every action is a plain HTML form. The decision record is [ADR 0018](decisions/0018-server-rendered-web-ui.md).

## Signing in

The UI needs a sign-in provider (OpenID Connect or GitHub, see [authentication.md](authentication.md#sign-in)) and `server.public_url`. An anonymous visitor is redirected to `/auth/login` and comes back to the page they asked for. Without a provider every page answers `401` with a page explaining that the UI needs sign-in; API tokens work only with the API and the CLI. The header shows the signed-in user and role and has a Sign out button.

There is no setting to turn the UI off: it is always mounted, and without sign-in nobody can use it.

## Pages

| Path | Shows |
| --- | --- |
| `/` | every configured app: the running version (commit, digest and state of the current deployment), the latest deployment, how many deployments wait and run, and app and global locks |
| `/apps/{app}` | the running version with its pin commit, the queue in run order with priority and an in-flight badge, active blocks, the deploy history 25 at a time (newest first, `?before=<id>` for older pages) with app commit, digest, pin commit, Argo CD revision, kind, migration flag, rollback-of and superseded-by links; and, for deployers, the deploy, rollback, block and lock forms |
| `/deployments/{id}` | summary (state, kind, requester and source, times, migration flag, failure reason), lineage, gate results grouped by phase with status, message, timings and evidence, an Attempts section when the engine retried the sync or recorded a diagnosis (see below), the event timeline (the newest 500 events, with a retry badge on `syncing → syncing` events), rollbacks and retries of this deployment, and for deployers a cancel form while it has not finished. Notifications link here ([notifications.md](notifications.md#links)) |
| `/locks` | the global lock, app locks, the 50 most recent lock events, and for deployers the global lock form and unlock buttons |
| `/audit` | the audit log, 50 entries a page, filtered by action and app, with each entry's detail |
| `/tokens` | API tokens and the forms to create and revoke them (admins only) |

Links to commits use the GitHub web host derived from `github.api_url`, the same as notification links: app commits link into the app's source repository, pin commits and Argo CD revisions that are commit SHAs into the GitOps repository. An Argo CD revision that is not a SHA (a chart version, several sources) is shown as text. There is no link into the Argo CD UI.

## Who may do what

| Action | Route | Role |
| --- | --- | --- |
| read every page except tokens | `GET /`, `/apps/{app}`, `/deployments/{id}`, `/locks`, `/audit` | `viewer` |
| deploy a commit | `POST /apps/{app}/deployments` | `deployer` |
| cancel a deployment | `POST /deployments/{id}/cancel` | `deployer` |
| roll back | `POST /apps/{app}/rollback` | `deployer` |
| block a version | `POST /apps/{app}/block` | `deployer` |
| unblock a version | `POST /apps/{app}/unblock` | `deployer` |
| lock or unlock an app | `POST /apps/{app}/lock`, `POST /apps/{app}/unlock` | `deployer` |
| take or release the global lock | `POST /locks/global`, `POST /locks/global/release` | `deployer` |
| list, create and revoke API tokens | `GET /tokens`, `POST /tokens`, `POST /tokens/{name}/revoke` | `admin` |
| static files | `GET /static/{file}` | none |

A signed-in user whose role is too low gets an HTML `403` page naming the role needed, and forms they cannot use are not shown. The deploy and cancel routes exist only while the engine runs (at least one app configured). `internal/web` registers every route from one table and its tests check the role of every route.

The actions call the same services as the API, so validation, conflicts and audit rows are identical:

- **Deploy** takes a full commit SHA that must be on the branch the app follows, an optional reason and an Urgent box ([api.md](api.md#manual-deploys-and-cancellation)). A deploy of a commit that is already queued or running opens that deployment.
- **Cancel** stops a deployment that has not finished, with an optional reason. When the deployment already pinned its version, the form warns that the version stays pinned in git until a rollback or a newer deploy replaces it.
- **Roll back** queues a rollback to the previous good version; "Roll back to this" on a successful deployment in the history picks that one. It opens a small form that names the commit and digest it re-pins and takes an optional reason, so a single click never queues a rollback. When the rollback would cross a release flagged as containing database migrations, nothing is changed and a confirmation page lists the target and every crossed migration deployment with its reason; confirming re-posts the same request with `confirm_migration_rollback=1` ([rollback.md](rollback.md#confirmation)).
- **Block** takes one reference, a `sha256:` digest or a full commit SHA (trimmed and lowercased), and a required reason, like `doorstep block <app> <digest|commit>`. It sits in a disclosure on the app page, below the live region. Blocking deploys, cancels and rolls back nothing; it only stops automatic deploys of that version, and rollbacks are never stopped. Blocking a version that is already blocked extends the existing block ([rollback.md](rollback.md#blocks)). Success shows the notice `blocked` or `block_exists`; an invalid reference or a missing reason re-renders the page at `400` with the typed values and the disclosure open.
- **Unblock**, **Unlock**, **Release the global lock** and **Revoke** open a disclosure that says what the action does, and act only on its confirm button.
- **Lock** needs a reason and takes an optional expiry such as `2h` ([locks.md](locks.md#web-ui)). The owner is the signed-in user. Locking and unlocking write `lock.acquire` and `lock.release` audit rows with the user and role, which the Audit page shows.
- **Tokens**: a new token is shown once, in the response to the form, with `Cache-Control: no-store`; it is never put in a URL or cookie. Token audit rows carry `origin.via: ui`.

A successful action answers `303 See Other` to the page that shows the result, with a fixed `?notice=` code (`deploy_queued`, `rollback_queued`, `locked`, ...) that the server maps to a message; unknown codes are ignored, so nothing from the URL is echoed into the page. A refused action renders the page again with the message and the values that were typed, at the matching status: `400` invalid input, `404` unknown app, deployment or commit, `409` blocked, not on the branch, already locked, rollback in progress or finished, `503` when GitHub or the database changed underneath and a retry may work.

## Retry attempts

The engine records every `syncing → syncing` retry and every failure with the data documented in [engine.md](engine.md#retry-before-rollback), and the deployment page turns it into an Attempts section between Gates and Events. The page decodes the event data itself and tolerates missing or unknown fields; data it cannot read leaves only the event's message. The heading says Attempts because Retries is the list of child deployments.

Each failed attempt has a block with "Attempt N of M", the time, the verdict as a text badge (`version`, `configuration`, `transient`, `unknown`), the summary, the Argo CD sync result and what happened next: "Retrying as attempt N+1 after 1m0s", or for the final failure "Decision: retries_exhausted" with a "rollback eligible" badge when automatic rollback may follow. Under it a findings table (signal, node, pods, reason, exit code, detail), the nodes, the evidence lines and, when pod diagnostics were off, a hint that only Argo CD messages were used. A deployment with no retries and no diagnosis has no section.

## Live refresh

Pages that change while you watch (the overview, app pages, a running deployment and the locks page) wrap their changing part in `<div id="live">`, which htmx fetches again every 5 seconds from the same URL and swaps in place (`hx-select="#live"`, `hx-swap="outerHTML"`). There are no fragment endpoints: a poll renders the whole page and htmx keeps only that region, so a poll and a reload can never disagree. A deployment page stops polling once the deployment has finished.

Actions that depend on the state shown sit inside the refreshed region, so they never go stale: the lock or unlock panel and the per-row unblock and "Roll back to this" disclosures on an app page, the cancel form of a deployment (which disappears with the last poll once the deployment finishes), and the global lock or release form and per-app unlocks on the locks page. While you use the region, `app.js` skips the poll: as long as a `<details>` in it is open, a field in it has focus, or a text field in it holds text. Rows therefore cannot move under the cursor while a confirm button is showing, and typing is never interrupted. The deploy form and the app-wide rollback form do not depend on the state, so they sit below the region, and so does the Block form.

The Attempts section of a deployment page is inside the region and deliberately has no disclosures: an open `<details>` pauses the poll, and a collapsed diagnosis would hide what someone watching a retry wants to see. Its contents are bounded by the engine (10 findings, 10 nodes, 5 evidence lines).

When a session expires, a poll answers `401` with an `HX-Redirect` header to the sign-in page (with the current page as `return_to`), and `app.js` reloads the page on any other `401`. Polls are logged at `debug` rather than `info` ([configuration.md](configuration.md#doorstep-serve)).

Everything works without JavaScript: forms post and redirect, destructive actions sit in `<details>` disclosures (with a reason field where the action records one: rollback, block and cancel) instead of `confirm()` dialogs, and only the live refresh and local time tooltips are lost. A test checks that every rollback, block, unblock, unlock, release, revoke and cancel form is inside a disclosure and that every rollback and block form has a reason field.

## Accessibility

Times are rendered as `<time datetime="...">` in UTC with a relative label ("5 minutes ago"); `app.js` adds the local time as a tooltip. States and gate statuses are text badges, never colour alone. Tables have captions and header scopes, inputs have labels, there is a skip link and a visible focus ring, and `app.css` follows `prefers-color-scheme`. The refreshed region is deliberately not an ARIA live region, so screen readers are not interrupted every 5 seconds; notices use `role="status"` and errors `role="alert"`.

## Security

- Every page is sent with `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`, plus `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin` and `Cache-Control: no-store`. There is no inline script or style; a test fails if one appears.
- htmx is configured with a `htmx-config` meta tag: `allowEval` and `allowScriptTags` off, `selfRequestsOnly` on, `includeIndicatorStyles` off (no injected styles) and `historyCacheSize` 0 (no page snapshots in local storage).
- `html/template` escapes everything. Gate messages, evidence, commit data, lock reasons and audit details come from outside doorstep and are shown as text. Evidence is pretty-printed in a `<pre>`; only its `logs_url` and `url` keys become links, and only when they are absolute `http` or `https` URLs without user information (`rel="noopener noreferrer"`).
- CSRF protection is the authenticator's ([authentication.md](authentication.md#csrf-protection)): every form carries the session's `csrf_token`, the page body carries it for htmx as `hx-headers`, and cross-origin posts are refused. A test checks that every rendered `POST` form has the token, and tests drive the real authenticator through sign-in to prove missing, wrong and cross-origin tokens are refused.
- Diagnosis text (pod messages, node names, evidence lines) comes from the cluster and is shown as escaped text only, never as a link or HTML.
- Form bodies are limited to 1 MiB (`auth.MaxFormBytes`). The authenticator reads a signed-in browser's form with that limit when it checks the CSRF token, and the UI applies the same limit to any request the authenticator did not read.
- A post whose CSRF token is stale, for example from a tab opened before signing in again elsewhere, gets an HTML `403` page saying nothing was changed and to reload and retry, not the API's JSON error ([authentication.md](authentication.md#csrf-protection)).

## Static files and vendored htmx

`internal/web/static` holds `app.css`, `app.js` and `htmx.min.js`, embedded into the binary. They are served at `/static/{file}?v=<first 12 hex characters of the file's sha256>`; a request with the current hash is cacheable for a year (`immutable`), any other is `no-cache` with an `ETag`. Only `.css` and `.js` files are served.

### Vendored htmx

htmx is vendored, not loaded from a CDN, so the UI works offline and the code that runs is reviewed. `static/VERSIONS` records the version, licence (Zero-Clause BSD, text in `static/htmx.LICENSE`), source and sha256 of `htmx.min.js`, and `TestVendoredFilesMatchVersions` fails when the file and the record disagree. The current version is htmx 2.0.11.

To update it:

1. Download the npm tarball of the new 2.0.x release, check it against the registry's `integrity`, and extract `package/dist/htmx.min.js` and `package/LICENSE`.
2. Replace `internal/web/static/htmx.min.js` and `htmx.LICENSE`, and update the version, source URL and `sha256sum` in `static/VERSIONS`.
3. Read the release notes for changes to configuration keys, `hx-select`, `hx-trigger="every"` and `HX-Redirect`, then run `just check`.

## Limitations

- Every poll renders the whole page. The overview reads locks, current and latest deployments and the queue in four queries whatever the number of apps, and an app page in five; the rendered page still grows with the catalog. That is fine for a homelab's apps and viewers; it is not built for hundreds of apps.
- One environment per app, like the rest of doorstep; there is no environment switcher.
- There is no link into the Argo CD UI and no log streaming; gate evidence links (`logs_url`, `url`) lead to external logs.
