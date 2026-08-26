# Documentation

Start with the [README](../README.md) for what Waveoff is and why. These pages
are the normative detail.

## Reference

| Page | What it settles |
|---|---|
| [digest.md](digest.md) | What each digest covers, the classification map, the absent-versus-zero rules, and the attested-versus-asserted trust model |
| [diff.md](diff.md) | The `waveoff diff` output, its impact tags, its verdicts and its exit codes |
| [scoring.md](scoring.md) | The scorer contract — the seam where Waveoff consumes scores it does not produce |
| [instrumentation.md](instrumentation.md) | What the recorder captures, the cassette format, and how sessions correlate |
| [gating.md](gating.md) | The gate: what test runs when, what it assumes, and what it refuses to do |
| [shadow.md](shadow.md) | Write suppression, synthetic objects, egress confinement, and the limits of each |
| [troubleshooting.md](troubleshooting.md) | What the common failures look like and what they actually mean |

## If you are trying to

- **Understand what a rollback would have to undo** — [diff.md](diff.md)
- **Work out whether a change needs a canary** — [digest.md](digest.md); two
  manifests sharing a `behaviorDigest` are the same agent
- **Decide a margin** — [gating.md](gating.md). There is deliberately no
  default: it is a judgement about your product that nobody else can make
- **Run a shadow stage safely** — [shadow.md](shadow.md), including the
  precondition the injection webhook refuses to proceed without
- **Plug in your own eval vendor** — [scoring.md](scoring.md)
- **Contribute** — [CONTRIBUTING.md](../CONTRIBUTING.md)

## Worked examples

`config/samples/` carries one of each: a sealed manifest pair, a registry
migration, an offline-replay rollout, a staged rollout that goes replay →
shadow → live, the shadow deployment itself, and the egress policy its
annotation attests to. `make quickstart` walks the manifest half of that on a
throwaway cluster.

## Honesty about maturity

The README's status table says which layers ship and which have never seen
production agent traffic. The gate's statistics are correct within their
assumptions and tested against generated data with known effect sizes; no
production traffic has been through them. Calibrate margins against outcomes you
can already observe before letting anything roll back unattended.

Two limits are worth reading before running a shadow or live stage rather than
after: a suppressed write always succeeds, so shadow does not see a candidate
that regressed only in failure handling ([shadow.md](shadow.md)); and a live
canary is unpaired, so it needs roughly three times the traffic a shadow stage
does to reach the same resolution ([gating.md](gating.md)).
