# Changelog

This project follows [Semantic Versioning](https://semver.org). While the API
is `v1alpha1`, breaking changes to the CRD may land in minor releases — but
never without a conversion plan and a migration note here. A digest that
changes meaning is treated as a breaking change even when the schema does not,
because it silently invalidates every manifest already issued.

## Unreleased

## v0.1.0 — 2026-08-27

The first release. Manifests, recording, replay, and the rollout controller.

Every layer below ships and is tested; the gate, shadow and live stages have
**never seen production agent traffic**. Read the "Maturity" section before
letting any of them roll anything back unattended.

### Project

- **Contributor documentation.** `CONTRIBUTING.md` (setup, what review looks
  for, the conventions), `RELEASING.md`, `SECURITY.md` (what this project is
  responsible for, and the limits it holds deliberately),
  `docs/troubleshooting.md` and a `docs/` index.
- **Agent guidance.** `CLAUDE.md` records the invariants and the vocabulary;
  `.claude/rules/` covers what a digest change costs, what CEL can express,
  what the gate assumes, which test suite a change belongs in, and the
  licensing boundary.
- **`make quickstart`** brings up a throwaway cluster and walks pin → seal →
  apply → diff, so the loop can be seen before anything is installed.
- **Every GitHub Action is pinned to a commit SHA** with the readable tag kept
  in a trailing comment. `make check-pins` fails the build on a mutable tag;
  `make update-pins` re-resolves them. A tag can be moved, which is the same
  hazard this project refuses in a container image.
- **`make lint-go`** with a deliberately small golangci-lint selection, and a
  spell check over the prose with `project-words.txt` for the vocabulary a
  general dictionary does not know.
- **Workflow hardening.** No workflow-level write grant; no expression
  interpolation inside shell scripts; no `pull_request_target`; checkout no
  longer persists the job token into `.git/config`. Release artefacts carry
  GitHub build-provenance attestations, verifiable with
  `gh attestation verify`. `hack/check-pins.sh` enforces all of it, and
  SECURITY.md states plainly what is still trusted and by whom.
- **Dependencies** brought current before the release: Kubernetes libraries to
  0.36.4 and controller-runtime to 0.24.1, whose generic webhook builder needed
  the registration call rewritten; all six GitHub Actions bumped, each pin
  verified against the tag it claims.
- **CI** gained concurrency groups, path filters and named jobs, with a
  companion workflow that reports the same check names for documentation-only
  changes — a skipped workflow reports no status at all, which would block
  every docs pull request forever.

### Added

#### Manifests

- **`AgentManifest` CRD** (`waveoff.ai/v1alpha1`) pinning everything that
  determines an agent's behaviour: code, model and decoding parameters,
  prompts, tool contracts, retrieval, policy bundle and judge configuration.
- **Two digests.** `behaviorDigest` covers what determines behaviour;
  `contentDigest` covers the whole spec. Two manifests sharing a
  `behaviorDigest` are the same agent, so a registry migration promotes without
  a canary while still being recorded. See [docs/digest.md](docs/digest.md).
- **`waveoff diff`** — plane-grouped comparison built for an on-call engineer,
  with three verdicts (`identical`, `provenance only`, `behavioural change`)
  mapped onto exit codes 0/1/2, and `-o json` for CI.
- **`waveoff pin`** — builds a manifest from a running Deployment. Resolves the
  image digest from the running pod rather than the pod template, reads model
  configuration from the container environment, hashes prompt bodies out of
  mounted ConfigMaps, and pins tool contracts from live MCP servers.
- **`waveoff verify`** — recomputes both digests and the object name with no
  cluster required; `--write` repairs a file in place without reformatting it.
- **Validating admission webhook** running `failurePolicy: Fail`, which
  recomputes both digests, requires a digest-pinned image, requires an operator
  -asserted `effect` on every tool, and freezes `waveoff.ai/evidence.*`
  annotations.

#### Recording

- **Recorder sidecar** on two chokepoints in one process: the model plane as an
  OpenAI/Anthropic-compatible proxy, and the tool plane as an MCP proxy. Both
  are reverse proxies, so the agent needs no code change.
- **Injection webhook** that adds the sidecar as a native sidecar (an init
  container with `restartPolicy: Always`, so the kubelet starts it before the
  agent) and rewrites the agent's base URLs to loopback. Injection is *skipped
  with a reason* rather than refused, so a webhook problem never blocks a
  deployment.
- **Cassette format** (`waveoff.ai/cassette/v1alpha1`), NDJSON, shaped after
  OpenTelemetry spans so a cassette stays a self-describing file and loads into
  a trace backend. Large bodies are offloaded to a content-addressed store.
- **Credential redaction at record time.** `Authorization`, `X-Api-Key`,
  cookies and a configurable regex set are stripped before a record exists.
  Cassettes are meant to be safe to commit and safe to hand to a vendor.
- **Session correlation by W3C trace context**, with `X-Waveoff-Session` as the
  escape hatch for runtimes that do not propagate it.
- **OpenTelemetry export**, off unless an operator configures an endpoint.
- **Upstream connection pooling** sized for a sidecar. `http.DefaultTransport`
  keeps two idle connections per host, so a reverse proxy inheriting it
  re-handshakes on every concurrent call past the second — tens of milliseconds
  against an external provider, on the component in front of every model call.
- Measured overhead: **p50 +0.13ms, p99 +0.33ms** on two cores, against the 5ms
  p99 budget §9 calls an adoption blocker.
  
  The budget is asserted by a test that takes the median of repeated
  measurements, because a single p99 over a few hundred requests is an order
  statistic one scheduler hiccup decides — and a gate on an adoption blocker
  that cries wolf is one people learn to re-run. Its concurrency scales to the
  machine, because the budget is a claim about what recording adds to one call
  and that cannot be measured on a saturated box: eight in-flight requests on
  two cores reported +1.2ms for the same code, which describes the hardware
  rather than the recorder.

#### Replay

- **`waveoff replay`** drives a candidate against recorded sessions with the
  model live and tools served from the recording.
- **Contract-drift detection** compares the tool contract a session was
  recorded against with what the server advertises now, so a corpus reports its
  own staleness instead of rotting silently.
- **Divergence reporting** — where the candidate's path left the recording, and
  whether what remains is usable as a measurement.

#### Gating

- **`AgentRollout` CRD** with three stage modes — `offline-replay`, `shadow`
  and `live` — and a controller that advances them one at a time.
- **Paired bootstrap** for fixed-horizon stages and an **empirical-Bernstein
  confidence sequence** for anytime-valid ones. A live stage must use the
  sequential test and the CRD enforces it: measured against generated data, a
  fixed-horizon test peeked at continuously produced a 12.5% false-regression
  rate where the confidence sequence produced 0.0%.
- **Non-inferiority testing** against an operator-chosen margin. There is
  deliberately no default margin.
- **Guardrails** by intersection–union test, decisive regardless of the primary
  metric. No Bonferroni correction on top: the IUT already controls the error
  and the correction would be pure loss.
- **Differential-missingness check** (drop-rate ceiling plus a McNemar exact
  test), because an item scored under one arm and not the other is evidence.
- **Judge calibration gate.** Every number the gate compares came from a judge;
  a judge whose agreement has drifted, or was measured against a different judge
  model, produces something that looks like evidence.
- **`waveoff rollout`** runs a rollout's gates on a laptop, with no cluster.
- **Scorer contract** with two transports — a subprocess and an HTTP endpoint —
  and no vendor SDKs. Waveoff does not evaluate agents; it consumes scores.
- **Analyzer seam**: the in-process analyzer can be replaced by any endpoint
  that answers the same request.

#### Shadow and live

- **Write suppression.** In a shadow pod, every tool classified as writing is
  answered without being executed and every unclassified tool is refused.
- **Synthetic object registry**, session-scoped, so an agent can read back what
  a suppressed write claimed to create. IDs are deterministic, so a retried call
  yields the same object rather than a second one.
- **Egress-confinement precondition.** The injection webhook refuses to inject a
  shadow sidecar unless the pod attests that its egress is confined.
- **Suppressed writes are recorded**, carrying the contract digest the server
  advertised — so contract-drift detection covers write tools, which are the
  ones it matters most on and the ones a shadow stage never executes.
- **Write-divergence guardrail**: the candidate attempted a class of write the
  incumbent never makes. Deterministic, needs no scorer, and fires on the first
  session.
- **Traffic routers** for Gateway API and Istio, behind one interface.
- **Session stickiness is a hard requirement** of a live stage. A router that
  cannot demonstrate session affinity holds the stage rather than running it.
- **Automatic rollback**, off by default, with per-trigger opt-in and the
  incumbent retained so a withdrawal is a weight flip rather than a rebuild.

### Design notes

- **Digests are authored, not computed at admission.** There is no defaulting
  webhook: one would leave Argo CD and Flux showing permanent drift, and a
  digest minted server-side is a weaker compliance claim than one committed to
  git.
- **`effect` is never inferred from MCP server hints.** `readOnlyHint` and
  `destructiveHint` are the server's claims about itself, and the server is the
  untrusted party in the tool-poisoning threat model this manifest exists to
  detect. `waveoff pin` renders them as a comment and refuses to adopt them.
- **A tool's `contractDigest` covers its description text**, not just its JSON
  schema, because the description is prompt input.

### Known limitations

- The `waveoff.ai/evidence.*` annotation freeze is enforced only by the
  webhook. It cannot be backed by a CEL rule, because root-level CRD validation
  expressions cannot reference `metadata.annotations`. Unlike spec
  immutability, it does not survive the webhook being unavailable.
- Prompt, policy and rubric digests are *asserted*, not attested: the content
  lives elsewhere and admission cannot verify it. Only `code.image@sha256:` is
  self-verifying. See the trust model in [docs/digest.md](docs/digest.md).
- `waveoff pin` cannot introspect stdio MCP servers, only HTTP ones, and cannot
  find prompts baked into a container image.
- **A suppressed write always succeeds.** Real writes sometimes fail — a
  validation error, a rate limit, a conflict — so a shadow stage systematically
  over-measures a candidate that regressed only in failure handling. It never
  met an error to handle. Not fixable by synthesising failures: choosing a
  failure rate means inventing one, and an invented failure is not evidence
  about how the candidate handles real ones.
- **Egress confinement is a pod-level guarantee.** A NetworkPolicy selects pods,
  not containers, and the sidecar shares the agent's pod — so the MCP servers it
  must reach are reachable by the agent directly, round the suppressor.
  Databases, object storage, queues and arbitrary HTTP are closed; that one path
  is not. See [docs/shadow.md](docs/shadow.md) for the two ways to close it.
- **A live canary is unpaired and cannot be made otherwise.** Each session is
  served by one arm, so there is no counterpart to subtract. The interval is
  about 1.8x wider than a paired one on the same data — roughly three times the
  sessions for the same resolving power. The unpaired construction is also
  deliberately conservative: it never over-promotes and assumes nothing about
  how the samples relate.
- **Session affinity on Gateway API needs the experimental channel.**
  `sessionPersistence` does not exist in the standard channel, and the API
  server prunes it silently — the apply succeeds and the field is gone.
- **Repeated measurements of one corpus item are refused**, not accepted.
  Resampling them as independent understates variance and over-promotes; a
  clustered bootstrap is not implemented, so the shape that would silently break
  the test is rejected.
- **Four controller-runtime APIs in use are deprecated** and acknowledged at
  each call site: `scheme.Builder`, `GetEventRecorderFor`,
  `admission.CustomDefaulter` and `webhook.CustomValidator`. The typed
  `Validator[T]` and `Defaulter[T]` replacements would remove casts and are
  worth taking, but the event-recorder migration changes the Event objects the
  controller emits — and `WavedOff` is something operators alert on — so all
  four belong in a change of their own rather than inside a dependency bump.
- **Process-group kill is Unix-only.** A scorer or replayed agent is usually a
  wrapper around something slower, and killing only the direct child leaves the
  grandchild holding the output pipes — so the timeout does nothing and a
  hanging judge hangs the rollout it was meant to protect. On Unix the child
  gets its own process group and the group is signalled. Windows has no group
  to signal, so cancellation kills the direct child only and a `WaitDelay`
  keeps a stuck grandchild from blocking forever. A full guarantee there needs
  a Job Object.
- **Per-rollout scorer credentials are not configurable.** The HTTP scorer uses
  the manager's own environment. A namespace-scoped Role is the documented
  middle path and is not implemented.

### Maturity

The manifest, recorder and replay layers have been exercised against real
infrastructure throughout: a real API server, a real kind cluster, the official
MCP reference server and a real LangGraph agent.

The gate, the shadow stage and the live canary **have never seen production
agent traffic.** Their statistics are correct within their assumptions and
tested against generated data with known effect sizes, but the margins are
uncalibrated by definition — a margin is a judgement about a product, and no
product has yet judged.

Calibrate against outcomes you can already observe before letting any of this
roll anything back unattended. `spec.rollback.auto` defaults to off for exactly
this reason.
