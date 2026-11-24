# Installing on Kubernetes

doorstep ships a [kustomize](https://kustomize.io) base under `deploy/kustomize`. You write a small overlay that pins an image digest and supplies your configuration file; the base provides everything else.

| Path | Purpose |
| --- | --- |
| `deploy/kustomize/base` | The install base: ServiceAccount, RBAC, ConfigMap, Deployment and Service |
| `deploy/kustomize/components/registry-credentials` | Optional component that mounts registry credentials |
| `deploy/kustomize/examples/homelab` | A complete example overlay with a namespace and an example configuration file |

## Prerequisites

- Kubernetes with [Argo CD](https://argo-cd.readthedocs.io) installed.
- Postgres 15 or newer that doorstep can reach; see [Postgres](#postgres) and [database.md](database.md).
- A GitHub App, set up as described in [github-app.md](github-app.md).
- `kubectl` (which includes kustomize through `kubectl apply -k`) or a standalone `kustomize`.

## What the base creates

| Object | Name | Purpose |
| --- | --- | --- |
| ServiceAccount | `doorstep` | Identity for the pod; its token is mounted into the pod and used to read Argo CD Applications through the Kubernetes API ([argocd.md](argocd.md#in-the-cluster)) |
| ClusterRole | `doorstep-applications-reader` | `get`, `list` and `watch` on `applications.argoproj.io`, nothing else |
| ClusterRoleBinding | `doorstep-applications-reader` | Binds the ClusterRole to the ServiceAccount |
| ConfigMap | `doorstep-config-<hash>` | The configuration file, generated from `config.yaml` |
| Deployment | `doorstep` | One replica running `doorstep serve` |
| Service | `doorstep` | ClusterIP with port `http` (8080) and port `metrics` (9090) |

It deliberately does not create a Namespace, any Secret, Postgres, an Ingress, a NetworkPolicy or a ServiceMonitor. The base sets no `namespace`, so use it only from an overlay that sets `namespace:`; the ClusterRoleBinding subject needs a namespace, which kustomize fills in from the overlay, and the API server rejects the binding without one.

## Quick start

Create the namespace and the Secrets. The Secret `doorstep` is mounted as a directory at `/etc/doorstep/secrets`, and each key becomes a file there:

```sh
kubectl create namespace doorstep
kubectl -n doorstep create secret generic doorstep \
  --from-literal=database-url='postgres://doorstep:CHANGE_ME@postgres.example.svc:5432/doorstep?sslmode=verify-full' \
  --from-file=github-app-private-key.pem=./doorstep.private-key.pem \
  --from-literal=github-webhook-secret="$(openssl rand -hex 32)" \
  --from-literal=slack-webhook-url='https://hooks.slack.com/services/T000/B000/XXXX'
```

If you use the registry credentials component (the example overlay does), create its Secret too:

```sh
kubectl -n doorstep create secret docker-registry doorstep-registry \
  --docker-server=ghcr.io --docker-username=robot --docker-password="$GHCR_TOKEN"
```

Copy `deploy/kustomize/examples/homelab` into your GitOps repository, then:

1. Edit `config.yaml`: the GitHub App ID and your apps and notification targets (see [configuration.md](configuration.md)).
2. Replace the example image digest in `kustomization.yaml` (see [Pinning the image](#pinning-the-image)).
3. Point the overlay at a release instead of the relative paths, using the same tag for both:

```yaml kubernetes
resources:
  - namespace.yaml
  - github.com/sharpintelligence/doorstep//deploy/kustomize/base?ref=vX.Y.Z
components:
  - github.com/sharpintelligence/doorstep//deploy/kustomize/components/registry-credentials?ref=vX.Y.Z
```

4. Apply it:

```sh
kubectl apply -k .
```

The example namespace is labelled `pod-security.kubernetes.io/enforce: restricted`; the Deployment meets that standard.

## Secret keys

All keys live in the Secret `doorstep`. Nothing in the base generates or contains a Secret; create it with `kubectl`, SealedSecrets, External Secrets or whatever you already use.

| Key | File | Required | Used by |
| --- | --- | --- | --- |
| `database-url` | `/etc/doorstep/secrets/database-url` | yes | `DOORSTEP_DATABASE_URL_FILE`, set by the base |
| `github-app-private-key.pem` | `/etc/doorstep/secrets/github-app-private-key.pem` | for GitHub | `github.private_key_file` in `config.yaml` |
| `github-webhook-secret` | `/etc/doorstep/secrets/github-webhook-secret` | for GitHub webhooks | `github.webhook_secret_file` in `config.yaml`; the same value as the App's webhook secret ([github-app.md](github-app.md#webhooks)). Leave the key and the setting out for a polling-only install |
| `slack-webhook-url` | `/etc/doorstep/secrets/slack-webhook-url` | for Slack | `webhook_url_file` of a notification target |

Other notification secrets follow the same pattern: add a key to the Secret and reference `/etc/doorstep/secrets/<key>` from its `*_file` setting.

Notification secrets and registry credentials are re-read when used, so rotating them needs no restart. The database URL, the GitHub App private key and the GitHub webhook secret are read once at startup: after changing any of them, restart the Deployment with `kubectl -n doorstep rollout restart deployment/doorstep`.

## Pinning the image

The base references `ghcr.io/sharpintelligence/doorstep` without a tag and sets its digest through the kustomize `images` transformer. The digest in the base is a placeholder of zeros: it is valid syntax, and fails with an obvious pull error if you forget to override it, rather than silently pulling `latest`.

Override it in your overlay with the digest of a release:

```yaml kubernetes
images:
  - name: ghcr.io/sharpintelligence/doorstep
    digest: sha256:<digest>
```

Find the digest in the GitHub Release notes, or with `crane digest ghcr.io/sharpintelligence/doorstep:vX.Y.Z`. Verify the signature before deploying; the exact `cosign verify` and `gh attestation verify` commands are in [releasing.md](releasing.md#verifying-an-image).

The `images` entry is the shape doorstep's own kustomize pin strategy writes, so doorstep can deploy itself from the GitOps repository that holds this overlay.

## Configuration

The configuration file is the `config.yaml` next to your `kustomization.yaml`, delivered through a `configMapGenerator` named `doorstep-config`. The base ships an empty one (`apps: []`); an overlay replaces it with `behavior: replace`, as the example does. The file is mounted at `/etc/doorstep/config/config.yaml` and selected with `DOORSTEP_CONFIG`.

doorstep reads the file once at startup. The generator appends a hash of the content to the ConfigMap name, so any change to `config.yaml` changes the pod template and rolls the Deployment automatically; there is no manual restart.

Validate the file in your GitOps repository's CI before it ships:

```sh
doorstep config validate --config config.yaml
```

## Postgres

The base only consumes a connection URL from the Secret key `database-url`. It does not run Postgres. The URL looks like `postgres://doorstep:<password>@<host>:5432/doorstep?sslmode=verify-full`; use `verify-full` with `sslrootcert` across untrusted networks. See [configuration.md](configuration.md#database) for the parameters and [database.md](database.md) for the privileges doorstep needs. Migrations need no Job: `serve` applies them before opening its listener, serialised by an advisory lock. The startup probe allows 300 seconds (150 attempts, 2 seconds apart) for connecting and migrating; raise `failureThreshold` in your overlay if a migration needs longer.

If an operator already generates the credentials, point the volume at its Secret instead. For a CloudNativePG cluster named `pg`, whose `pg-app` Secret has the URL under `uri`:

```yaml kubernetes
patches:
  - target:
      kind: Deployment
      name: doorstep
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: doorstep
      spec:
        template:
          spec:
            containers:
              - name: doorstep
                env:
                  - name: DOORSTEP_DATABASE_URL_FILE
                    value: /etc/doorstep/database/database-url
                volumeMounts:
                  - name: database
                    mountPath: /etc/doorstep/database
                    readOnly: true
            volumes:
              - name: secrets
                secret:
                  secretName: doorstep
                  defaultMode: 0440
                  items:
                    - key: github-app-private-key.pem
                      path: github-app-private-key.pem
                    - key: github-webhook-secret
                      path: github-webhook-secret
                    - key: slack-webhook-url
                      path: slack-webhook-url
              - name: database
                secret:
                  secretName: pg-app
                  defaultMode: 0440
                  items:
                    - key: uri
                      path: database-url
```

The patch matches the env entry and the volume by name, so it does not depend on the order in which components add entries.

## RBAC

The base grants `get`, `list` and `watch` on `applications.argoproj.io` through a ClusterRole and ClusterRoleBinding, because each app names its own Argo CD namespace (`argocd.namespace`) and Argo CD can host Applications in any namespace. doorstep uses `get` on one Application and `watch` on the Applications of its namespace filtered by name; it never lists across namespaces, never patches or syncs Applications, and the grant includes no Secrets. Without `watch`, doorstep still works but falls back to reading the Application every 30 seconds.

doorstep reads the mounted ServiceAccount token (`/var/run/secrets/kubernetes.io/serviceaccount/token`) for every request, so bound token rotation needs no restart. After installing, `kubectl exec deploy/doorstep -- doorstep argocd status <app> --config /etc/doorstep/config.yaml` checks the permissions and connection; see [argocd.md](argocd.md#doorstep-argocd-status).

To restrict doorstep to one namespace, which keeps working because doorstep only reads Applications in the namespaces its apps name, delete the ClusterRoleBinding in your overlay and bind the same ClusterRole with a RoleBinding in the Argo CD namespace.

```yaml kubernetes
patches:
  - patch: |-
      $patch: delete
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRoleBinding
      metadata:
        name: doorstep-applications-reader
```

Apply the RoleBinding from a separate kustomization, because an overlay-wide `namespace:` would move it out of the Argo CD namespace:

```yaml kubernetes
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: doorstep-applications-reader
  namespace: argocd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: doorstep-applications-reader
subjects:
  - kind: ServiceAccount
    name: doorstep
    namespace: doorstep
```

## Registry credentials

The component `components/registry-credentials` mounts the Secret `doorstep-registry` (type `kubernetes.io/dockerconfigjson`) at `/etc/doorstep/registry` and sets `DOORSTEP_REGISTRY_CREDENTIALS_FILE`. It is optional because `serve` refuses to start when a credentials file is configured but unreadable; for public images, leave it out. Matching rules are in [configuration.md](configuration.md#registry-credentials).

## Exposing the HTTP port

The Service has two ports. Route only `http` (8080) from an Ingress or Gateway; never route `metrics`.

```yaml kubernetes
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: doorstep
spec:
  rules:
    - host: doorstep.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: doorstep
                port:
                  name: http
```

The only path GitHub needs to reach is `POST /webhooks/github`; it authenticates every delivery by its signature, so it is safe to expose without sign-in. To keep the rest of doorstep private, route just that path publicly (`path: /webhooks/github`, `pathType: Exact`) and serve the UI and API on an internal ingress. When GitHub cannot reach doorstep at all, leave the webhook inactive: the poller picks up new commits within `intake.poll_interval` ([configuration.md](configuration.md#intake)).

The base ships no Ingress or NetworkPolicy, because both depend on your controller and CNI.

## Metrics

Prometheus metrics are served on the separate `metrics` port, see [metrics.md](metrics.md). With the Prometheus Operator, a ServiceMonitor selects the Service by label and scrapes that port:

```yaml kubernetes
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: doorstep
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: doorstep
  endpoints:
    - port: metrics
```

## Security posture

The pod meets the restricted Pod Security Standard: non-root user 65532, `RuntimeDefault` seccomp, no privilege escalation, all capabilities dropped and a read-only root filesystem. The service writes nothing to disk, so there is no writable volume. The only Kubernetes permission is the read-only Applications grant above.

## Scaling

The base runs one replica, which is the right default for a homelab. More replicas are safe, because queue claims and locks are coordinated in Postgres; set `replicas` in your overlay and add a PodDisruptionBudget at the same time. The base has none, since a budget with one replica would block node drains.

## Upgrades

Bump the `?ref=` of the base and component and the image digest together, then apply. Migrations run on start and are forward-only, so a rollout of a newer version cannot be undone by deploying the older image against the migrated database.

## Validating changes

`just lint-manifests` renders the base and every example with `kustomize build` and checks the result with `kubeconform`; CI runs it. A Go test in `deploy/kustomize` additionally renders the base and every example in-process and checks the result (unique volume names, the config volume pointing at the generated ConfigMap, namespaced binding subjects), and checks that the manifests stay consistent with the code, for example that environment variables are known settings and that the example configuration validates. See [development.md](development.md).
