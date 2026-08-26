<!-- Copyright 2026 The Waveoff Authors. SPDX-License-Identifier: Apache-2.0 -->

# Shadow stages

A shadow stage mirrors production traffic to a candidate that serves no user.
The candidate sees real requests; nothing it produces reaches anybody.

That is only true if the candidate cannot change the world, and mirroring alone
does nothing to stop it. A candidate receiving mirrored traffic will do
everything it would normally do — file the ticket, send the email, charge the
card. Without suppression, "shadow" means "we hope the candidate behaves", and
the first time it does not, a shadow deployment writes to production twice.

## How writes are suppressed

The recorder sidecar puts a suppressor **in front of** the MCP proxy, not inside
it: a call that has already reached the server cannot be un-made.

Every `tools/call` is looked up in the manifest's effect classification:

| Effect | What happens |
|---|---|
| `read` | forwarded |
| `idempotent-write`, `write` | answered without being executed |
| unclassified or absent | **refused** |

Refusal for an unclassified tool is the same fail-closed rule replay uses. A
tool nobody has classified is exactly the one that might write, and letting it
through on the grounds that we do not know is the reverse of what the mandatory
`effect` field is for. The injection webhook refuses a shadow pod whose manifest
classifies nothing at all, for the same reason: safe-and-useless and working
look identical from outside.

## Read-your-writes

A suppressed write returns a **synthetic object** with an identifier, minted
from `sha256(session, tool, arguments)` and remembered for the session.

This matters more than it sounds. An agent that has just created something will
use its id — to comment on it, to link to it, to read it back. A result with no
id sends a well-built agent straight down an error path, which is then measured
as a regression the candidate did not cause. So:

- the id is returned both as a structured `id` field and in the text, because
  tools differ in where a caller looks for what was created;
- a subsequent call whose arguments mention a synthetic id anywhere is answered
  from the registry rather than forwarded, so the agent does not watch its own
  work disappear;
- a retried call with identical arguments yields the **same** id, so a retry
  does not look like a second object;
- the registry is scoped to one session and evicted by TTL. Nothing another
  session did is visible.

## Stated limitation: synthesised success is always the happy path

A suppressed write always succeeds. Real ones sometimes do not — a validation
error, a rate limit, a conflict, a 500.

So a shadow stage **systematically over-measures a candidate that regressed only
in failure handling.** If the candidate's new retry logic is wrong, or its
error-path prompt is worse, shadow will not see it: it never met an error to
handle. The stages that follow are where that surfaces.

This is not fixable by making the suppressor cleverer about which failures to
synthesise. Choosing a failure rate means inventing one, and an invented failure
is not evidence about how the candidate handles real ones. It is stated here
rather than worked around.

## Precondition: egress must be confined

Suppression covers what goes through the MCP proxy. It sees nothing else:

- plain HTTP the agent makes directly,
- database drivers,
- object-storage SDKs,
- message queues,
- the filesystem.

A shadow pod that can reach any of those has a **partial guarantee presented as
a complete one**, and the failure mode is a candidate quietly writing to
production while a dashboard says it is only observing.

So the injection webhook refuses to inject a shadow sidecar unless the pod
carries `waveoff.ai/egress-confined: "true"`. That annotation is an attestation,
not a check — nothing in a webhook can verify that a NetworkPolicy exists and
covers everything. It is a deliberate obstacle, so that confining egress is a
decision somebody made rather than something nobody thought about.
`config/samples/shadow-egress.yaml` is the policy it attests to.

### What the policy closes, and what it does not

A NetworkPolicy selects **pods, not containers**, and the sidecar runs in the
agent's pod. So the destinations the sidecar needs — the MCP servers it proxies
to, and the model provider — have to be allowed at pod level, which means the
agent container can reach them directly as well, going round the suppressor.

**Closed:** every other egress path. Databases, object storage, queues,
arbitrary HTTP APIs, the internet. That is the large majority of the ways a
shadow candidate could write to production.

**Not closed:** the agent talking to an allowed MCP server itself, without the
sidecar in between.

Two ways to close the rest, in order of preference:

1. Per-container egress policy — a sidecar-aware mesh, or an eBPF policy engine
   that can distinguish processes. If your platform has it, use it instead.
2. Point the shadow deployment at **staging** MCP servers rather than production
   ones, so a direct call writes somewhere nobody minds. This needs no new
   machinery and it changes what the candidate sees, which is a trade worth
   making deliberately.

The first draft of the sample policy was a flat default-deny, which is stricter
and completely useless: it cut off the sidecar too, leaving a shadow stage that
was perfectly safe and measured nothing. `test/e2e/shadow_test.go` now asserts
both halves — that the policy confines the agent, and that the sidecar still
answers through it — along with the gap above, so that it cannot be quietly
forgotten.

## Suppressed writes are the best signal a shadow stage produces

They are recorded, not just logged. A suppressed call goes into the cassette
like any other step, through the same annotator, marked
`waveoff.tool.suppressed: true` and carrying the effect the manifest asserted.

Three things follow:

1. **Nothing mistakes a placeholder for an effect that happened.** The attribute
   is on the span; a reader that ignores it is choosing to. A call *refused*
   for want of a classification is marked `waveoff.tool.refused` instead, and
   deliberately not as a suppression: a suppressed write is evidence about the
   candidate, a refusal is evidence about the manifest, and they need different
   fixes.
2. **Contract-drift detection covers write tools.** The suppressed call carries
   the contract digest the server advertised, so the one class of tool a shadow
   stage never executes is no longer the one class drift never sees — and write
   tools are where drift matters most.
3. **The attempts are directly comparable across arms.** The candidate's writes
   did not happen, so no scorer can compare their effects; but both arms saw the
   same traffic and each decided what to reach for. That comparison is the
   `write-divergence` guardrail — deterministic, needing no judge, and available
   on the first session. See [gating.md](gating.md).

A shadow stage where nothing was ever suppressed either had no write tools or
was not actually suppressing, and those look identical from outside. The
counters are reported for that reason.
