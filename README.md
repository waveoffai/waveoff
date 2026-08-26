# Waveoff

**Rollback for agents, made unambiguous.**

A version of an agent is not a container image. It is a composite — code,
prompts, model pin, tool contracts, retrieval snapshot, policy bundle, judge
config — living in five systems on five different cadences. Nobody versions it
atomically, so "roll back the agent" has no single answer, and at 2am somebody
is guessing which of five things moved.

Waveoff pins all of it into one immutable, content-addressed object, and makes
the difference between two of them legible.

```console
$ waveoff diff support-agent-a875c0c15289 support-agent-b7ed494b4261

  support-agent-a875c0c15289 → support-agent-b7ed494b4261      3 planes changed · 4 unchanged

  model      pin           2026-05-01 → 2026-08-01
             temperature   0.2 → 0.7
                           ↳ affects model output

! tools      jira.create_issue
               contract      3333… → 9999…
               ↳ description or schema changed · effect=write
               ↳ affects model input · security-relevant
             slack.post_message  ADDED
               ↳ effect=write  replayPolicy=no-op
               ↳ new tool can write
               ↳ affects model input · security-relevant

! judges     task-completion
               model         claude-opus-4-1 → claude-opus-5
               ↳ κ=0.71 measured 2026-08-01, against the OLD judge — stale
               ↳ affects how the GATE scores

  unchanged  code · prompts · retrieval · policy

  behavioural change — 4 changes reach the model, 2 changes move the gate itself
```

## Install

Download a binary from [releases](https://github.com/waveoffai/waveoff/releases),
or:

```console
go install github.com/waveoffai/waveoff/cmd/waveoff@latest
```

Every release ships a `SHA256SUMS` file. Verifying it is worth the ten seconds
if you are the kind of shop that cares who built your tools:

```console
sha256sum -c waveoff_v0.1.0_SHA256SUMS --ignore-missing
```

`waveoff diff`, `verify` and `pin --help` need no cluster. To install the CRD
and the admission webhook — which does need one, plus
[cert-manager](https://cert-manager.io) — apply the rendered manifest from a
release:

```console
kubectl apply -f https://github.com/waveoffai/waveoff/releases/download/v0.1.0/waveoff-v0.1.0.yaml
```

Or just the CRD, if you want `waveoff pin` and `kubectl get agentmanifests`
without admission checks yet:

```console
kubectl apply -f https://github.com/waveoffai/waveoff/releases/download/v0.1.0/waveoff-crds-v0.1.0.yaml
```

Installing the CRD without the webhook means digests are checked by the CRD's
own validation rules but not recomputed, so a manifest whose digest does not
cover its spec would be accepted. `waveoff verify` catches that in CI; the
webhook is what catches it in the cluster.

## Start here

If you would rather see the whole loop before installing anything, one command
brings up a throwaway cluster, installs Waveoff, and walks pin → seal → apply →
diff:

```console
make quickstart
```

Otherwise:

```console
# Build a manifest from what is actually running.
waveoff pin deployment/support-agent -n prod \
  --mcp-server jira=https://jira-gw.internal/mcp \
  -o manifest.yaml

# Resolve the TODOs it left you, then seal it.
waveoff verify --write manifest.yaml

kubectl apply -f manifest.yaml
```

`pin` resolves the image digest from the pod that is running, not from the pod
template — the template usually carries a mutable tag, and a manifest that
cannot say which bytes ran is not a release artifact. Given `--mcp-server`, it
connects to each server and pins the tool contracts it advertises.

## Recording, and the one thing it asks of your agent

The recorder is a sidecar proxy. An injection webhook rewrites your agent's base
URLs to point at it, so **capturing traffic needs no code change at all**.

Correlating a multi-step agent loop into one *session* does. The recorder
derives a session from W3C trace context, so an agent that does not propagate it
gets one session per HTTP call — which still supports payload-level replay, and
does not support divergence detection or task-level comparison. That is two
lines of OpenTelemetry bootstrap, and if you already run LLM observability you
almost certainly have it. See [docs/instrumentation.md](docs/instrumentation.md)
for what you lose without it and how to check.

## Three ideas worth knowing

**Two digests, not one.** `behaviorDigest` covers what determines behaviour.
`contentDigest` covers everything. Same behaviour digest means the same agent, so
a registry migration promotes with no canary — while the move is still recorded,
still diffed, and still covered by a hash. See [docs/digest.md](docs/digest.md).

**A tool's contract digest covers its description text, not just its schema.**
The description is prompt input. That makes a silent description rewrite both a
behavioural change and a detectable tool-poisoning attempt, and it is why this
manifest doubles as a security control.

**`effect` is mandatory and never inferred.** MCP servers advertise
`readOnlyHint` and `destructiveHint`; `waveoff pin` shows them to you as a
comment and refuses to adopt them. The server is the untrusted party in the
threat model this object exists to detect. An unclassified tool fails closed.

## Deploying

```console
make install   # the CRD
make deploy    # the webhook (requires cert-manager)
```

The controller lives in `waveoff-system`. To put it somewhere else, set the
namespace in `config/default/kustomization.yaml` — everything else derives from
it:

```console
cd config/default && kustomize edit set namespace waveoff-prod
```

Do this rather than wrapping `config/default` in an overlay that sets
`namespace:`. Two references — the webhook certificate's SANs and
cert-manager's `inject-ca-from` annotation — are strings embedded inside other
values, so they are derived by kustomize replacements rather than by the
namespace transformer, and those replacements resolve before an outer overlay's
transformer runs. An overlay would move the resources and leave those two
strings pointing at the old namespace, giving a webhook that cannot complete a
TLS handshake — which under `failurePolicy: Fail` rejects every AgentManifest in
the cluster. `make check-namespace` proves the supported path keeps working.

## Design decisions you may disagree with

**Digests are authored, not computed at admission.** There is no defaulting
webhook. A mutating webhook that rewrote the stored object would leave Argo CD
and Flux showing permanent, un-reconcilable drift, and this is a GitOps-native
product. `waveoff verify --write` fills them in; the cluster only ever checks
them. It also makes the stronger compliance claim: the digest an auditor reads
was committed to git, not minted server-side.

**The webhook runs `failurePolicy: Fail`, with no namespace exclusions.** Digest
verification is the entire security property of this object, and with `Ignore`
it is bypassable by taking the webhook down. The usual advice to exclude
`kube-system` applies to webhooks intercepting core resources; this one matches
only `agentmanifests`, which no namespace needs in order to boot, so an
exclusion would create a bypass and protect against nothing.

**A cross-agent diff is refused, not rendered.** It produces a verdict-shaped
answer that means nothing, and somebody will act on it.

## What this is not

Not an eval framework. Eval-in-CI is a solved, crowded category and Waveoff
consumes scores from whichever vendor you already use. Not an MCP gateway — it
sits adjacent to one and works without one. Not an agent framework.

## Status

`v1alpha1`.

| Layer | State |
|---|---|
| Manifests — CRD, digests, `pin`, `diff`, `verify` | ships |
| Recorder — sidecar, cassettes, corpus, injection | ships |
| Replay — `waveoff replay`, contract drift, divergence | ships |
| Gating — `AgentRollout`, offline-replay stage, paired-bootstrap gate | ships, **not yet validated against production traffic** — see [docs/gating.md](docs/gating.md) |
| Shadow mode — write suppression, synthetic objects, write-divergence guardrail | ships, **not yet validated against production traffic** — see [docs/shadow.md](docs/shadow.md) |
| Live canary, auto-rollback, traffic routing | ships, **not yet validated against production traffic** |

The gate's statistics are correct within their assumptions and tested against
generated data with known effects. No production agent traffic has been through
them. Calibrate the margins against outcomes you can already observe before
letting one roll anything back unattended.

Two limits are worth reading before running a shadow or live stage rather than
after: a suppressed write always succeeds, so shadow does not see a candidate
that regressed only in failure handling ([docs/shadow.md](docs/shadow.md)); and
a live canary is unpaired, so it needs roughly three times the traffic a shadow
stage does to reach the same resolution ([docs/gating.md](docs/gating.md)).

## Samples

`config/samples/` has a worked example of each shape:

| File | What it shows |
|---|---|
| `incumbent.yaml`, `candidate.yaml` | two versions of one agent, sealed |
| `registry-migration.yaml` | same `behaviorDigest`, different content — promotes without a canary |
| `rollout.yaml` | offline replay only: nothing in production is involved |
| `rollout-staged.yaml` | replay → shadow → live canary, with auto-rollback |
| `shadow-pod.yaml` | the candidate deployment a shadow stage mirrors to |
| `shadow-egress.yaml` | the egress policy the shadow annotation attests to |

## Documentation

| Page | What it settles |
|---|---|
| [docs/digest.md](docs/digest.md) | What each digest covers, and the attested-versus-asserted trust model |
| [docs/diff.md](docs/diff.md) | The diff output, its impact tags, verdicts and exit codes |
| [docs/gating.md](docs/gating.md) | The gate: which test runs when, what it assumes, what it refuses to do |
| [docs/shadow.md](docs/shadow.md) | Write suppression, synthetic objects, egress confinement — and the limits of each |
| [docs/scoring.md](docs/scoring.md) | The scorer contract, where Waveoff consumes scores it does not produce |
| [docs/instrumentation.md](docs/instrumentation.md) | What the recorder captures and how sessions correlate |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Failures whose symptom is a long way from their cause |

[docs/README.md](docs/README.md) is the index. [CONTRIBUTING.md](CONTRIBUTING.md)
covers setup and what review looks for; [SECURITY.md](SECURITY.md) covers what
this project is responsible for and what it deliberately is not;
[RELEASING.md](RELEASING.md) covers cutting a version.

## Development

```console
make            # generate, vet, boundary checks, tests, build
make test       # unit, property and API-server tests
make lint-go    # golangci-lint
make help       # every target, with what it does
```

Every GitHub Action is pinned to a commit SHA, with the readable tag kept in a
trailing `# ratchet:` comment. A tag is mutable — the same `@v4` can mean
different code tomorrow — which is the argument this project makes about
container images in a manifest, and it would be an odd position to hold in the
README while not holding it in CI. `make check-pins` enforces it and
`make update-pins` re-resolves them.

`make test-envtest` downloads a real API server and exercises the round trip
through it. That is where the interesting failures live: values do not go from a
YAML file straight into a hash, they pass through an API server that re-decodes
every number and through server-side apply that owns fields.

```console
make test-e2e   # needs Docker
```

The e2e suite installs cert-manager, the Gateway API CRDs (experimental channel
— `sessionPersistence` exists nowhere else) and the Istio CRDs into a throwaway
kind cluster, then exercises the webhook, the recorder sidecar, write
suppression under a real NetworkPolicy, and both traffic routers against a real
API server. It is where the wiring failures live: a mirror filter written
without a port, a NetworkPolicy that confined the sidecar along with the agent,
and a CRD channel that silently drops the field a live canary depends on were
all found here and nowhere else.

`KEEP_CLUSTER=1` leaves the cluster up to poke at; `USE_EXISTING=1` runs against
whatever `kubectl` already points at.

Agent-facing guidance lives in [CLAUDE.md](CLAUDE.md), with the detailed rules
in [`.claude/rules/`](.claude/rules/) — what changing a digest costs, what CEL
can and cannot express, what the gate assumes, and which test suite a change
belongs in.

## Licence

Apache 2.0.

There is no feature gating in this repository — no `pro` build tag, no
`enterprise/` directory, no stub that errors with an upgrade prompt — and
[`hack/check-boundary.sh`](hack/check-boundary.sh) fails the build if any
appears. There is no telemetry, phone-home or version check either. Both are
enforced in CI rather than promised in prose.

Recording volume is never metered or capped. The store keeps whatever the
operator gives it disk for.
