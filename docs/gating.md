# Gating

An `AgentRollout` compares a candidate manifest against the incumbent and
decides whether it may be promoted.

## Status: implemented, not validated against production traffic

The statistics here are correct within their stated assumptions and the code is
tested against generated data with known effects. **No production agent traffic
has ever been through this gate.** Two of the design's claims are empirical
rather than mathematical and remain unverified: how much pairing actually
reduces the required sample size depends on the between-arm correlation of real
traffic, and the right non-inferiority margin for any metric is a judgement
about a real product.

Treat a verdict from this gate as evidence about your corpus, and calibrate the
margins against outcomes you can already observe before letting it roll anything
back unattended.

## Alpha is two-sided; the test is one-sided at half of it

`alpha: 0.05` builds a **two-sided 95% interval**, and the non-inferiority test
reads one end of it. That is a one-sided test at 0.025 — the usual
non-inferiority convention, and the more conservative of the two readings. If
you want a one-sided 0.05 test, set `alpha: 0.10`.

## The offline-replay stage

The only stage that exists today. Shadow mode and live canary come later; a mode
that is not implemented is absent from the enum rather than accepted and
ignored.

Both arms replay **the same recorded sessions, in the same order, from the same
recordings.** That is what makes the comparison paired, and pairing is what
makes the sample size manageable — so it is a property of how the stage runs and
not something you can switch off.

Nothing touches production: every tool read is served from the recording and
every write is refused.

## One primary metric

```yaml
gate:
  primary:
    metric: task-completion
    test: paired-bootstrap
    direction: higher-is-better
    margin: -0.02
    alpha: 0.05
```

Exactly one metric carries the promotion decision, at the full alpha. Gating
eight metrics at 0.05 each gives roughly a 34% chance that one fires by accident
— which shows up as a rollback of a perfectly good release, and then as a team
that stops trusting the gate.

Everything else is a guardrail — and guardrails run at **full alpha, with no
multiplicity correction**.

That is not an oversight. Every metric here is a non-inferiority test: the null
is "this is breached" and the candidate has to prove otherwise. Promotion
requires rejecting the primary null *and* every guardrail null, which is an
intersection–union test, and the size of one is bounded by the largest of its
parts. Requiring all *k* rejections already controls the false-promotion rate at
alpha.

Correcting on top of that would only shrink alpha, widen every interval and make
guardrails harder to satisfy — costing power to prove non-inferiority and waving
off good releases, to buy error control the structure already provides.

The correction *would* be needed under the opposite framing, where a guardrail
is a detection test with a null of "no harm". That framing has a second problem:
an underpowered guardrail then passes by default, which is the reverse of what a
gate should do.

### Non-inferiority, not superiority

The test asks "is the candidate not worse by more than δ?", never "is it
better?". Most releases are neither better nor worse in any measurable way, and
a superiority test would block all of them.

`margin` is δ. There is deliberately no default: how much regression is
acceptable is a judgement about your product, and a number we invented would be
worse than no number at all.

For `higher-is-better`, a margin of `-0.02` tolerates a two-point drop. For
`lower-is-better` — cost, escalation rate — the comparison flips, and a margin
of `0.15` tolerates a 15% increase.

### Why paired bootstrap

**Paired** because item difficulty varies enormously between tasks and cancels
out entirely in the per-item difference. **Bootstrap** because it assumes
nothing about the distribution, which matters for judge scores bounded in
`[0,1]` where a t-test's normality assumption is plainly false.

### Pairing is a property of the deployment, not a setting

An offline replay and a shadow stage are paired: both arms are driven from the
same recorded session, so item difficulty cancels in the per-item difference.

**A live canary is not, and cannot be made to be.** Each session is served by
one arm; there is no counterpart to subtract. The gate detects this from the
stage mode and switches to an unpaired interval — two independent sequences,
each at half the level, combined by their worst corners.

The cost is real and worth stating plainly: on identical data the unpaired
interval is about **1.8× wider** than the paired one, which is roughly three
times as many sessions for the same resolving power. A live stage therefore
takes longer to conclude than a shadow stage over the same traffic, and that is
a fact about what a canary can observe rather than a deficiency in the test.

The unpaired construction here is also deliberately conservative. Splitting
alpha across two arms and taking the worst corners never over-promotes, and it
never assumes anything about how the two samples relate — which in a live canary
is only "whoever happened to be routed where". Sharper unpaired intervals exist;
they buy width by assuming more. This one buys correctness by assuming less, and
for a test whose job is to decide whether to ship, that is the right side to err
on.

### Session stickiness is a hard requirement of a live stage

Weights are evaluated **per request**. A multi-turn agent session routed by
weight alone lands on both arms across its turns, which corrupts the measurement
— an observation attributed to one arm was partly produced by the other — and
breaks the agent outright if either arm holds session state.

So a `TrafficRouter` must report its stickiness, and a live stage on a router
that cannot demonstrate session affinity is **held, not started**. The router
must key on the agent's own session identifier (`X-Waveoff-Session`), which is
the same unit the recorder derives a session from — so what the router keeps
together is what the analysis counts.

On Gateway API this means `sessionPersistence` on the rule, which exists **only
in the experimental channel**. On a standard-channel install the API server
prunes the field: the apply succeeds, nothing warns, and the field is simply
gone — after which a live stage holds forever, telling the operator to configure
affinity they already did configure. `test/e2e/traffic_test.go` asserts both
behaviours against a real API server, and `hack/e2e.sh` installs the
experimental channel for that reason.

Keying on a cookie or a source address is not sufficient and is treated as not
sticky. A cookie identifies a browser and there is no browser here; a source
address, for a fleet of agent pods behind one egress, identifies everything.
Either would route by something correlated with tenant, region or time of day,
and no amount of correct interval arithmetic recovers from two arms serving
different populations.

## Guardrails are decisive

No improvement in the primary metric buys a policy violation. A guardrail
failure waves the candidate off regardless of everything else.

### The write-divergence guardrail

One guardrail is deterministic and needs no scorer at all: **the candidate
attempted a class of write the incumbent never makes.**

The suppressor records every write it declines to forward, so a shadow stage
produces a per-arm set of tool names and a count for each. Comparing the two
sets is free, it is available on the first session, and it fires on **set
membership rather than on a rate**. The incumbent has been serving this traffic
in production, so its write classes are the demonstrated normal; a candidate
reaching outside that set has changed what the agent *does*, not how often it
does it. Waiting for such a difference to reach statistical significance means
waiting for a destructive call to happen repeatedly first, which is the wrong
way round.

Rate differences *within* the shared set are reported and deliberately not
judged here. "Three times as many tickets" might be a regression or a busier
hour, and separating those is exactly what the primary metric and its interval
are for.

A write class the candidate has **stopped** using is reported and never fires.
A candidate that writes less may have got better or may have got lazier, and
only a scorer can tell those apart.

The trigger is `write-divergence` and can be excluded from
`spec.rollback.triggers` like any other. The finding is still reported on the
stage status when it is.

## What the gate refuses to do

**Promote on an unmeasured metric.** A metric nobody scored is not a metric that
passed.

**Decide when the interval cannot separate pass from fail.** Where the
confidence interval sits relative to the margin gives three answers, not two:
entirely on the acceptable side is non-inferior, entirely on the other side is
genuinely worse, and **straddling the margin is `Inconclusive`**. That replaces
any argument about a minimum sample size — it adapts to the metric and its
variance, which is what the honest minimum actually depends on. A separate floor
of `BootstrapFloor` items guards the resampling itself, since below a few dozen
items a percentile bootstrap draws from too few distinct values for its interval
to mean anything however narrow it looks.

**Treat an undecidable guardrail as a passing one.** An underpowered guardrail
is an unmeasured one wearing a p-value.

**Impute a missing arm.** An item scored under only one arm is dropped and
counted. Substituting a mean or a zero invents data exactly where a paired test
is most sensitive to it.

**Trust a drop pattern it has not checked.** Dropping is only unbiased when the
missingness is random, and here it very likely is not: scorers fail more on long,
complex, edge-case sessions, and they can fail at different rates between the
arms. The path that matters is a candidate producing more malformed or truncated
output, making the judge choke on it more, having those items dropped, and then
being promoted on the subset where it behaved — shipping exactly the regression
the gate exists to catch, while looking like a clean pass.

So two things are tested before any number is believed:

- the **overall drop rate**, against a ceiling (20% by default). A fifth of the
  corpus failing to score is not a smaller sample of the same population, it is
  whatever the scorer could handle.
- the **asymmetry between arms**, by an exact McNemar test on the discordant
  pairs. Items both arms scored, or neither did, say nothing about which arm is
  harder to judge; all the evidence is in the items one arm scored and the other
  did not.

Either failing gives `Inconclusive`, not a verdict.

**Treat a failure as a verdict.** A scorer that errors, an analyzer that is
unreachable, a corpus whose contracts have drifted — all of these hold the
rollout and report why. Failing open on a promotion decision ships the exact
change nobody was able to check.

**Hide how much was measured.** Status records sessions attempted, items scored
and items excluded, because a gate that scored 40 of 400 items otherwise looks
identical to one that scored all 400.

## Repeated measurements are refused, for now

The analyzer resamples at the item level. If one corpus item appears more than
once — repeated runs of the same task, as `pass^k` would produce — the request
is **rejected** rather than accepted.

Resampling repeats as if they were independent understates variance, narrows the
interval and over-promotes. Repeated measurement needs a clustered bootstrap:
resample items, then repeats within item. That is not implemented, so the shape
that would silently break it is refused instead of quietly mishandled.

## What makes a verdict recomputable

A fixed seed makes the resampling deterministic **given the scores** — and the
scores come from a judge that is not deterministic. So the verdict carries the
per-item observations it was computed from, and records the seed it used rather
than only fixing it in code. The analysis can then be rerun without invoking the
judge again, and a future change to seeding is visible in the evidence rather
than invisible in a constant.

## Bringing your own test

Statistical practice for non-deterministic systems is not settled. If you have a
statistician who wants a different procedure, implement one endpoint:

```yaml
analyzer:
  endpoint: http://my-analyzer.internal/analyze
```

It receives an analysis request as JSON and returns a verdict — the same types
the built-in analyzers use, so there is exactly one document to read. The
controller does not know or care which implementation answered.

An analyzer that returns an outcome this build does not recognise is rejected
rather than guessed at, and one that returns `promote` with no observations
behind it is refused.

## Vocabulary

A failed gate **waves off** a candidate; it does not "fail" or "abort" it. The
wave-off is the decision and the rollback is the implementation, and the API
keeps the two words apart because they are different things: you can wave off a
candidate that was never rolled out, and you can roll back for reasons that have
nothing to do with a gate.
