<!-- Copyright 2026 The Waveoff Authors. SPDX-License-Identifier: Apache-2.0 -->

# Troubleshooting

Failures whose symptom is a long way from their cause. Most of these were found
the hard way; the entry says what the message actually means.

## Manifests

### Every `AgentManifest` in the cluster is rejected

The webhook is not reachable, and it runs `failurePolicy: Fail` on purpose —
digest verification is the security property, and `Ignore` would make it
bypassable by taking the webhook down.

```console
kubectl get deployment -n waveoff-system waveoff-manager
kubectl get validatingwebhookconfiguration validating-webhook-configuration \
  -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | head -c 20
```

An empty CA bundle means cert-manager's ca-injector has not run. Check that
cert-manager is installed and that the `Certificate` in `waveoff-system` is
ready. Manifests are not on the request path, so nothing user-facing is down —
you cannot create or update manifests until it is fixed.

### `behaviorDigest does not match the spec it claims to cover`

Expected. Digests are authored, not computed at admission — nothing rewrites
what you applied, which is what keeps Argo CD and Flux from showing a permanent
un-reconcilable diff.

```console
waveoff verify --write manifest.yaml
```

If it happens on a manifest you did not edit, the digest classification changed
in a release. That is a breaking change and `CHANGELOG.md` will say so.

### `name must be derived from spec.agent and behaviorDigest`

`waveoff verify --write` fixes it. If two manifests legitimately share a
`behaviorDigest` — a registry migration — the disambiguated form
`<agent>-<behavior[:12]>.<content[:8]>` is also accepted.

### `image must be digest-pinned`

A tag is mutable, so a manifest carrying one cannot say which bytes ran and is
not a release artefact. `waveoff pin` resolves the running digest from the pod
status, not from the Deployment spec — the spec routinely carries a tag.

## Recorder

### No cassettes appear

The recorder writes a cassette header as soon as the first record arrives and
the spans follow asynchronously, so "a cassette exists" and "the recording
happened" are different questions.

```console
kubectl exec <pod> -c waveoff-recorder -- /waveoff-recorder \
  -corpus-dir /var/lib/waveoff/corpus -list
kubectl exec <pod> -c waveoff-recorder -- /waveoff-recorder \
  -corpus-dir /var/lib/waveoff/corpus -dump <session-id>
```

The image is distroless, so there is no shell: the binary answers for itself.
If `-list` is empty, the agent's traffic is not reaching the sidecar — check
that the webhook rewrote its base URL (`waveoff.ai/injected` on the pod).

### The sidecar was not injected

Look at the pod:

```console
kubectl get pod <pod> -o jsonpath='{.metadata.annotations.waveoff\.ai/inject-skipped}'
```

Injection is *skipped* with a reason rather than refused, so a webhook problem
never blocks a deployment. Common reasons: no upstream could be determined; the
manifest named does not exist; a shadow pod with no tool classifications; a
shadow pod without `waveoff.ai/egress-confined`.

### `Session terminated` from an MCP client

Usually a path problem rather than a session problem. The proxy strips its route
prefix; an exact-match route that does not strip it forwards `/mcp/mcp/<label>`
upstream, and the 404 that comes back surfaces as a terminated session.

### The agent hangs with no error

Check the JSON-RPC ids. A replayed or synthesised response carrying the
*recorded* id rather than the id the client just sent leaves the client waiting
for a reply that will never come. This is a hang, not a failure, which is what
makes it hard to see.

### One session became many

Session correlation is by W3C trace context. An agent that does not propagate
`traceparent` gets a synthetic session per call. Either instrument the agent or
set `X-Waveoff-Session` explicitly — that header is the escape hatch.

## Shadow

### Injection refused: egress is not confined

Deliberate. Write suppression covers what goes through the MCP proxy and nothing
else — direct HTTP, database drivers, object storage, queues and the filesystem
all bypass it. Apply an egress policy (`config/samples/shadow-egress.yaml`) and
set `waveoff.ai/egress-confined: "true"` to attest to it. Read
[shadow.md](shadow.md) for what that does and does not close.

### A shadow stage measures nothing

If the counters show nothing suppressed, either the agent has no write tools or
suppression is not actually running — those look identical from outside, which
is why the counters exist.

If the agent cannot complete a session at all, the egress policy may have cut
off the sidecar as well as the agent. The sidecar genuinely needs to reach the
MCP servers it proxies; a flat default-deny is stricter and useless.

### The candidate looks better than it is

Expected and documented. A suppressed write always succeeds, so a candidate that
regressed only in failure handling will not show it — shadow never met an error
to handle. See "synthesised success is always the happy path" in
[shadow.md](shadow.md).

## Rollouts

### A live stage is Held with "without session affinity"

Weights are evaluated per request, so a multi-turn session would be served by
both arms and the comparison would attribute one arm's work to the other.

On Gateway API this needs `sessionPersistence` keyed on `X-Waveoff-Session` —
and that field exists **only in the experimental channel**. On a
standard-channel install the API server prunes it silently: your apply
succeeded, nothing warned, and the field is gone. Check:

```console
kubectl get crd httproutes.gateway.networking.k8s.io \
  -o jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/channel}'
```

### A live stage is Held with "gated by paired-bootstrap"

A canary is watched continuously and a fixed-horizon test is not valid under
repeated peeking — it will eventually call a good candidate a regression. Use
the sequential test.

### The gate says `inconclusive` rather than deciding

Not the same as a wave-off, on purpose. "We cannot tell" is not "it is worse",
and collapsing them either blocks good releases or promotes on a test that never
really ran. Usually: too few paired observations, or an interval that still
straddles the margin. Let it run longer, or reconsider the margin.

### A rollout stopped with a judge-calibration error

The gate gates itself. Every number it compares came from a judge, and a judge
with drifted agreement — or one measured against a *different* judge model than
the one about to run — produces something that looks like evidence. Re-measure
κ, or lower `gate.kappaFloor` deliberately and say why.

### Duplicate items were refused

Repeated measurements of one corpus item are not independent. Resampling them as
though they were understates variance, narrows the interval and over-promotes. A
clustered bootstrap is not implemented, so the shape that would silently break
the test is rejected rather than accepted.

## Development

### `make test-e2e` fails on NetworkPolicy enforcement

The suite fails rather than skips when a cluster's CNI does not enforce
NetworkPolicy, because a shadow stage on such a cluster is not safe and a green
suite must not say it is. Use kind (kindnet enforces), or a CNI that does.

### `kind load docker-image` fails with "content digest not found"

A multi-platform image in the local store carries an index and attestation
manifests kind cannot import. `docker save --platform linux/<arch>` first, then
`kind load image-archive`. `hack/e2e.sh` does this.

### CI fails on action pins

A GitHub Action is pinned to a tag rather than a commit SHA. `make update-pins`
re-resolves them; the resulting diff is a supply-chain change and is meant to be
reviewed as one.
