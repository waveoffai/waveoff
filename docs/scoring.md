# Scoring

Waveoff does not evaluate agents. Eval-in-CI is a solved, crowded category and
you already run one; what nobody owns is the layer after the score. So a score
arrives from outside, over the smallest interface that works.

## What a scorer receives

A replay writes what the candidate produced as a cassette of its own — the same
format a recording uses, so a scorer written against one works against the
other. Scorers are given *references* to those cassettes, not their contents: a
transcript can be megabytes, a run covers hundreds of them, and anything that
wants the bytes can read them from the corpus.

```json
{
  "schemaVersion": "waveoff.ai/score/v1alpha1",
  "refs": [
    {
      "item": "4bf92f3577b34da6a3ce929d0e0e4736",
      "arm": "incumbent",
      "session": "4bf92f3577b34da6a3ce929d0e0e4736.incumbent",
      "corpus": "/var/lib/waveoff/replays",
      "blobs": "/var/lib/waveoff/blobs",
      "manifest": "sha256:6b1d4e8f…"
    },
    {
      "item": "4bf92f3577b34da6a3ce929d0e0e4736",
      "arm": "candidate",
      "session": "4bf92f3577b34da6a3ce929d0e0e4736.candidate",
      "corpus": "/var/lib/waveoff/replays",
      "blobs": "/var/lib/waveoff/blobs",
      "manifest": "sha256:7f3a9c2b…",
      "degraded": true,
      "reason": "2 write(s) refused and synthesised"
    }
  ]
}
```

`item` is the pairing key, and the reason the whole comparison works. A paired
test needs the same input scored under both arms, and "the same input" is the
recorded session both replays were driven from — not the replay's own session
id, which differs by construction.

`degraded` says the replay did not run cleanly. A session where half the tools
were no-op'd can still be scored, but the number means something different, and
a gate that cannot tell the two apart is measuring noise.

## What a scorer returns

```json
{
  "results": [
    {
      "item": "4bf92f3577b34da6a3ce929d0e0e4736",
      "arm": "incumbent",
      "metrics": {"task-completion": 0.91, "cost-per-completed-task": 0.043}
    },
    {
      "item": "4bf92f3577b34da6a3ce929d0e0e4736",
      "arm": "candidate",
      "metrics": {"task-completion": 0.88, "cost-per-completed-task": 0.051},
      "metadata": {"judge": "claude-opus-5", "rubric": "sha256:55…"}
    }
  ]
}
```

Metric names are yours. A gate names one of them as its primary metric, so the
vocabulary is the customer's and not ours.

### A failed score is not a zero

The single most important rule here. If a judge times out, return an error for
that item:

```json
{"item": "4bf9…", "arm": "candidate", "error": "judge timed out after 30s"}
```

Not `{"metrics": {"task-completion": 0.0}}`. A judge that failed has not decided
the agent failed. Counting that as zero is how a gate rolls back a good release
because a scoring service was briefly slow.

Items scored under only one arm are **dropped**, and the count of dropped items
is reported. They are never imputed: substituting a mean or a zero for a missing
arm invents data at exactly the point where the test is most sensitive to it.

## Two transports, no vendor dependencies

There are deliberately no vendor SDK adapters in this repository. That would put
third-party dependencies in the OSS module for vendors we do not control, on
release cycles we do not follow, and would pick winners in a market that has not
settled. Instead there are two transports that any vendor plugs into.

### Subprocess

Refs on stdin, results on stdout. Anything that reads and writes JSON is a
scorer.

```yaml
scorer:
  exec:
    command: python
    args: [scripts/score_with_braintrust.py]
    timeout: 10m
```

A Braintrust adapter is about twenty lines:

```python
import json, sys
import braintrust

req = json.load(sys.stdin)
results = []
for ref in req["refs"]:
    transcript = read_cassette(ref["corpus"], ref["session"])   # your reader
    try:
        scored = braintrust.eval(transcript)
        results.append({
            "item": ref["item"], "arm": ref["arm"],
            "metrics": {"task-completion": scored.score},
            "metadata": {"judge": scored.model},
        })
    except Exception as err:                     # a failure, not a zero
        results.append({"item": ref["item"], "arm": ref["arm"], "error": str(err)})

json.dump({"results": results}, sys.stdout)
```

A Langfuse or Promptfoo adapter is the same shape with a different import.

The subprocess runs in its own process group and is killed as a group on
timeout. A scorer is usually a shell wrapper around something slower, and
killing only the direct child leaves the grandchild holding the output pipes —
so the timeout would be decorative and a hanging judge would hang the rollout it
was supposed to protect.

### HTTP

For a hosted eval service. Same wire shape, different front door.

```yaml
scorer:
  http:
    endpoint: https://evals.internal/score
    headers:
      Authorization: Bearer ${EVAL_TOKEN}
    timeout: 5m
```

A non-2xx response is an error, not an absence of scores. A scoring service
being down is an infrastructure failure rather than a verdict, and a gate must
hold rather than read empty results as a pass.

## Rejected values

Metrics arrive from a process this repository does not control, so they are
validated before they reach a gate. `NaN` and infinities are refused: a `NaN`
reaching a bootstrap produces a confidence interval of `NaN`, which compares
false against every threshold — a gate that silently never fires.
