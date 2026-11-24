# 0012: The deployment engine

- Status: Proposed
- Date: 2026-11-24

## Context

Every adapter a deployment needs existed (queue and runner, registry, GitOps pin writer, Argo CD watcher, gates, blocks, notifications, metrics), but nothing drove a deployment from `queued` to a final state. The engine has to do that across several replicas, survive restarts at any point, never wedge a target (one active deployment per target), keep rollbacks and urgent deploys moving, and leave hooks for retries (doorstep-ehl.6.2) and automatic rollback (doorstep-ehl.6.5). The claim orders in-flight deployments ahead of everything else for a target, and a `schedule` or `lock` gate can keep a deploy waiting for days. See [engine.md](../engine.md).

## Decision

1. **Before gates hold the deployment in `queued`.** The engine evaluates the `before` phase on a claimed but untransitioned deployment. Pending releases the entry with a 30 second delay; passed moves `queued → resolving`, which is when the deployment starts; failed moves `queued → failed`, a new transition in the `core` table.
2. **One handler run per claim, idempotent steps.** `Engine.Handle` runs admit, resolve, pin, sync and verify in a loop within one claim until the deployment is terminal, a step asks for a delayed release, or an error or cancellation stops it. Each step re-derives its work from the persisted deployment, so a re-claim resumes at the current state.
3. **Engine-owned port types, adapters in the composition root.** `engine` imports only `core` and the standard library. It declares `Claim`, `Result`, `Transition`, `PinRequest`, `PinResult` and `Outcome` and the ports `DeploymentStore`, `DigestResolver`, `PinWriter`, `SyncWatcher`, `GateEvaluator`, `BlockList`, `Notifier`, `Metrics` and `FailurePolicy`. `internal/cli` adapts `store`, `registry`, `gitops`, `queue` and `metrics`; `*argocd.Watcher`, `*gates.Evaluator`, `*rollback.Service` and `*notify.Dispatcher` satisfy their ports directly. There is no queue or locker port.
4. **Explicit error classification and per-state deadlines.** Adapters wrap errors in `engine.ErrPermanent` (fail now) or `engine.ErrImageNotReady` (retry every 15 s); Argo CD outcomes use the `core.ErrSync*` sentinels; everything else is transient and the runner backs off. A state conflict or a cancelled context stops the handler without failing anything. Retries are bounded by the time since the deployment entered its state, read from the event log (`store.StateEnteredAt`): resolving 15 minutes, pinning 45 minutes, syncing the app's sync timeout, which the engine also passes to the watcher as the remaining time with a 10 second floor.
5. **One failure choke point.** Every failure goes through one method that asks a `FailurePolicy` with the stage, class, reason, sync failure and gate outcome before the `→ failed` transition. The only action is `fail`; later beads add actions.
6. **Post-commit announcements.** The worker emits each `deploy.*` event after its own successful transition. `deploy.queued` and `deploy.cancelled`, whose transactions run outside the engine, are announced by `Engine.Queued` and `Engine.Cancelled` from small decorators in `cli` around intake's settlement and rollback requests, after those transactions commit. `Cancelled` also abandons the pull request of a cancelled deployment that had started.

## Options considered

- **Before gates in `resolving`.** Simpler state table, but a deployment waiting for a deploy window would be in flight and block rollbacks, urgent deploys and batching for its target for as long as the window is closed. Rejected.
- **`queued → resolving → failed` for a failed before gate.** No new transition, but it records a start that never happened (`started_at`, `deploy.started`, the started counter). Rejected in favour of `queued → failed`.
- **One step per claim.** Matches the runner contract equally well but costs four extra release and claim round trips per deployment and wake-up latency between steps, with no gain in resumability. Rejected.
- **Moving `store.Transition`, `queue.Claimed` and `gitops.Request` into `core`.** Avoids adapters but churns packages that other work edits in parallel and puts adapter-shaped types in the domain package. Rejected for thin adapters.
- **Retries inside each adapter.** Each adapter would invent its own policy and none would know the deployment's age. The engine owns retry policy, as the adapters' documentation already assumed.
- **Emitting `deploy.queued` inside the enqueue transaction.** It could announce a deployment that then rolls back. Rejected; a post-commit announcement may arrive after `deploy.started`, which is acceptable for best-effort notifications ([ADR 0006](0006-notifications-are-best-effort.md)).
- **Configuration keys for intervals and deadlines.** No one has asked for them; they are constants with `engine.Options` overrides for tests.

## Consequences

- A deployment held by a before gate is visible as `queued` with its gate results, not as in flight, and the newest commit wins when a deploy window opens.
- A dependency outage of any length cannot wedge a target: past its deadline the deployment fails with the last error and the queue moves on. The cost is a failed deployment that a person may have to queue again until retries (doorstep-ehl.6.2) exist.
- A crash between a transition and its notification loses that notification; the history is the durable record.
- A failed deploy that already pinned stays pinned until someone requests a rollback, until automatic rollback (doorstep-ehl.6.5) uses the failure policy hook.
- `serve` refuses to start with apps configured but without the GitHub App or a Kubernetes connection, so tests and installations that configure apps must provide both.
