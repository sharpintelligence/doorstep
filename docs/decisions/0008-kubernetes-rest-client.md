# 0008: A small Kubernetes REST client, one get and watch per wait

- Status: Accepted
- Date: 2026-11-24

## Context

After a pin lands, doorstep waits until the Argo CD `Application` is synced to a revision containing it and healthy. ADR 0001 rules out the Argo CD API and token: doorstep only reads `Application` resources through the Kubernetes API. The architecture document planned the client-go dynamic client with an informer.

What doorstep actually needs is narrow: read one named Application in one namespace, and be told when it changes, for at most one wait per app at a time. It needs in-cluster ServiceAccount auth in production and a kubeconfig for development, and it must keep the security posture already used for GitHub: credentials only to the configured host, no redirects, bounded reads, rotated credentials picked up without a restart.

## Decision

- `internal/kube` is a small typed REST client on `net/http` with no `k8s.io` dependency. It builds a connection from the in-cluster ServiceAccount (`KUBERNETES_SERVICE_HOST`/`PORT`, CA bundle, token file re-read on every request) or from an explicitly named kubeconfig, using a supported subset (server, CA, token or token file, client certificate). Exec and auth-provider plugins, basic auth, impersonation, `insecure-skip-tls-verify` and `proxy-url` are refused; `kubectl proxy` is the escape hatch. There is no implicit fallback to `$KUBECONFIG` or `~/.kube/config`.
- The transport sends credentials only when scheme and host match the configured API server and refuses redirects. The CA bundle is the only trust root when one is given. `GET` bodies are capped at 16 MiB, watch streams at 64 MiB, error messages at 512 bytes.
- `internal/argocd` decodes only the Application fields it uses into private structs. It has no informer, cache or background goroutine: each wait does a `GET`, then a `WATCH` filtered by `metadata.name` from the returned `resourceVersion` with a server-side timeout equal to the resync interval (30 s). Any watch problem (end of stream, `410 Gone`, decode error, transport error) falls back to a fresh `GET`, so the watch degrades to polling at the resync interval.

## Options considered

### Small REST client, get and watch per wait (chosen)

Two calls on one resource type are a few hundred lines, in the style of `internal/github`, which is also hand-written even though ADR 0001 names go-github. Nothing is held in memory between waits, there is no lifecycle (`Start`, `HasSynced`) for the engine to wire, it works unchanged when RBAC is narrowed to one namespace, and an RBAC error surfaces on the call that hit it. The cost is owning the kubeconfig subset and the watch loop, and not supporting credential plugins directly.

### client-go dynamic client with an informer

The standard choice, and it supports every kubeconfig. It brings `k8s.io/client-go`, `k8s.io/apimachinery`, `k8s.io/api` and their protobuf and OpenAPI dependencies into the binary and the vulnerability scan surface for one get and one watch. A cluster-wide informer caches every Application, including large status blobs, needs list permission across namespaces (or one informer per namespace) and a start and sync lifecycle. Exec plugins only matter on development machines, where `kubectl proxy` covers them.

### client-go typed `Get` and `Watch` without an informer

Smaller than an informer, but the dependency graph is the same, and doorstep would still decode into `unstructured` maps because it does not import the Argo CD module.

### Polling only

Simplest, but every wait would add up to one polling interval of latency after Argo CD reports healthy, or poll the API server often. The watch gives prompt updates, and the polling fallback keeps the same code correct when the watch fails.

## Consequences

- No new modules; `go.mod` is unchanged.
- Development against cloud clusters with exec-based auth goes through `kubectl proxy`.
- Engine code sees only `core` types (`core.SyncStatus`, `core.Health`, `core.SyncFailure`) and sentinels, never Kubernetes or Argo CD types.
- If doorstep ever needs more Kubernetes resources or a shared cache, this decision should be revisited in a new ADR rather than growing the hand-written client piecemeal.
