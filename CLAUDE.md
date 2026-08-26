# CLAUDE.md

Guidance for agents working in this repository. Read the invariants before
changing anything; they are the reasons the design is shaped the way it is, and
most of them were paid for by a bug.

## What this is

Waveoff is a progressive-delivery controller for non-deterministic agent
systems. Two primitives:

1. **`AgentManifest`** — an atomic, content-addressed object pinning everything
   that determines an agent's behaviour, so that "roll back the agent" is
   unambiguous.
2. **A replay-backed canary controller** — `AgentRollout` runs a candidate
   through offline replay, shadow and live stages, gated by a statistical test.

It is **not** an eval framework, **not** an MCP gateway, and **not** an agent
framework. It *consumes* scores from whatever you already run.

## Layout

```
api/v1alpha1/          the two CRDs
internal/digest/       canonicalisation, the classification map, projection
internal/diff/         `waveoff diff` — the output was designed before the schema
internal/pin/          `waveoff pin` — introspects a Deployment and live MCP servers
internal/recorder/     the sidecar: model plane + tool plane, cassettes, suppression
internal/replay/       replay driver, contract drift, divergence
internal/analysis/     the statistics: paired bootstrap, confidence sequences
internal/rollout/      the controller: stages, live runner, observations
internal/traffic/      Gateway API and Istio routers
internal/score/        the scorer seam — exec and HTTP, no vendor SDKs
cmd/waveoff            the CLI (the only user-facing binary)
cmd/manager            the controller and webhooks
cmd/waveoff-recorder   the sidecar
```

## Invariants (never violate)

1. **No telemetry.** No phone-home, no version check, no anonymous usage
   statistics, ever. `make lint-boundary` enforces it.
2. **Nothing is metered or gated.** Recording, replay and corpus size are never
   limited. There are no "Enterprise" strings in user-facing output.
3. **`effect` is mandatory and fails closed.** An unclassified tool is *refused*
   during replay and shadow, never passed through. A server's own
   `readOnlyHint` is a claim by the untrusted party and is never promoted to an
   asserted effect.
4. **Credentials are stripped at record time.** `Authorization`, `X-Api-Key`,
   cookies and a configurable regex set. A cassette must be safe to commit and
   safe to hand to a vendor.
5. **Digests are authored, never computed at admission.** There is no mutating
   webhook: one would make Argo CD and Flux show a permanent un-reconcilable
   diff, and it would mean the digest an auditor reads was minted server-side
   rather than committed to git.
6. **The validating webhook runs `failurePolicy: Fail`.** Digest verification is
   the security property; `Ignore` makes it bypassable by taking the webhook
   down.
7. **A failed gate must never fail open.** An analyzer that cannot answer holds
   or withdraws; it never promotes. Absence of a verdict is itself the problem.
8. **The projection is built field by field, never `json.Marshal`.** Struct tags
   are not part of the digest contract — adding `omitempty` must not silently
   change every digest ever issued.

## Vocabulary

- A failed gate **waves off** a candidate. It does not "fail" or "abort" it.
- The **wave-off** is the decision; the **rollback** is the implementation. You
  can wave off a candidate that was never rolled out, and roll back for reasons
  that have nothing to do with a gate. The Kubernetes Event reason is
  `WavedOff`.
- Prose is British English. Identifiers are not: the CRD field is
  `behaviorDigest`, the interface method is `Analyze`, the MCP notification is
  `notifications/initialized`. Do not respell those.
- The GitHub organisation handle in the module path exists only because the
  plain name was taken. It is legal in an import path and nowhere else — never
  in a user-facing string. Suffixed brand names leak fast and are painful to
  walk back. `hack/check-boundary.sh` greps for it, with no file exemptions,
  which is why this line names it indirectly.

## How work gets verified here

Unit tests are necessary and have never been sufficient. Every interesting bug
in this repository was found by running against real infrastructure:

| Suite | Command | Finds |
|---|---|---|
| Unit + property | `make test-unit` | logic, canonicalisation, statistics |
| API server | `make test-envtest` | CEL, pruning, SSA field ownership, float round-trips |
| Integration | `make test-integration` | a real LangGraph agent, the real MCP reference server |
| End-to-end | `make test-e2e` | a real cluster: webhook wiring, injection, NetworkPolicy, routers |

Examples of what only the outer two caught: a `RequestMirror` filter written
without a port (rejected by CEL, invisible to the fake client); a NetworkPolicy
that confined the sidecar along with the agent; a Gateway API channel that
silently prunes the field a live canary depends on; a JSON-RPC id that made a
client wait forever rather than fail.

**Prefer a test that would have caught the bug over a test that describes the
fix.**

## Detailed rules

- `.claude/rules/digests.md` — changing the classification map
- `.claude/rules/crd-changes.md` — CRD and CEL changes
- `.claude/rules/statistics.md` — the gate's assumptions
- `.claude/rules/tests.md` — which suite a change belongs in
- `.claude/rules/boundary.md` — the licensing and telemetry boundary
