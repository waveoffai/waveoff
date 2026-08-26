# `waveoff diff`

The audience is an on-call engineer at 2am who needs to know which of an agent's
behavioural inputs moved, and which of those could plausibly explain what they
are looking at.

Everything about the output is arranged around that: a fixed plane order so the
eye lands in the same place every time, explicit naming of what did *not*
change, one consequence annotation per element rather than per field, and a
one-line verdict at the bottom.

```
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

## Planes

Always in this order, always all seven accounted for:

`code · model · prompts · tools · retrieval · policy · judges`

The `unchanged` line is not padding. "Retrieval is unchanged" is a different
fact from "retrieval is missing from this output", and at 2am the difference
matters.

## Tags

| Tag | Meaning |
|---|---|
| `affects model input` | what the model sees changed — a prompt, a tool description or schema, the retrievable document set |
| `affects model output` | the model itself or its sampling changed |
| `affects execution` | what runs changed, without necessarily reaching the model |
| `affects how the GATE scores` | the yardstick moved, not the agent |
| `security-relevant` | the change is also a security event |

## The `!` marker

A plane is marked when it carries a change that must not be skimmed past:

- **A tool's contract digest moved.** The digest covers the description text as
  well as the schema, because the description is prompt input. A silent rewrite
  is the shape of an MCP tool-poisoning or rug-pull attack, so this is always
  flagged even when the change is innocent.
- **A tool's effect widened** (`read` → `idempotent-write` → `write`). The tool
  may now do more than it could.
- **A tool's server changed** while its contract did not. Same contract, and no
  guarantee of the same data.
- **The policy bundle changed.** What the agent is permitted to do moved.
- **Anything in the judges plane.** Gating on a moved yardstick is its own
  failure mode.
- **A judge changed without being re-calibrated.** The κ the gate is about to
  trust was measured against a judge that no longer exists.

Colour is only ever an accent. Every marked line also carries a literal `!`, so
pasting the output into a ticket loses nothing. `NO_COLOR` and a non-terminal
stdout both disable it.

## Verdicts and exit codes

| Exit | Verdict | Meaning |
|---|---|---|
| `0` | `identical` | Both digests match. |
| `1` | `provenance only` | Behaviourally identical. Promotes without a canary. |
| `2` | `behavioural change` | This needs a canary. |
| `64` | — | Usage error, including a refused cross-agent comparison. |
| `69` | — | Cluster or network unreachable. |
| `70` | — | Internal error. |

Errors live in the [sysexits](https://man.freebsd.org/cgi/man.cgi?sysexits) range
rather than at 3, because wrapper scripts routinely special-case small integers
and a tool that returns the same code for "different" and "broken" gets misread.

### Cross-agent comparisons are refused

If the two manifests have different `spec.agent` values, `waveoff diff` prints
what it found and exits `64` without producing a verdict. A cross-agent
comparison produces a verdict-shaped answer that means nothing, and somebody will
act on it. `--force` overrides.

## Inputs

Each side is resolved independently, so a file can be compared against a live
cluster object.

| Form | Resolves to |
|---|---|
| `support-agent-a875c0c15289` | an object in the cluster |
| `manifest.yaml` | a file |
| `manifests.yaml#support-agent-a875c0c15289` | one document from a multi-document file |
| `-` | stdin |

A diff between two local files needs no kubeconfig at all.

Prompt line counts (`+12 −3`) are rendered only when both prompt bodies resolve
from files reachable locally. The diff must never fail because a git remote is
down, so anything else degrades to showing digests.

## JSON

`-o json` emits a versioned, stable shape. CI will branch on it, so fields may be
added but never removed or repurposed without a version bump.

```json
{
  "schemaVersion": "waveoff.ai/diff/v1alpha1",
  "agentA": "support-agent",
  "agentB": "support-agent",
  "behaviorDigest": {"a": "sha256:…", "b": "sha256:…"},
  "contentDigest":  {"a": "sha256:…", "b": "sha256:…"},
  "verdict": "behavioural",
  "changes": [
    {
      "plane": "tools",
      "element": "jira.create_issue",
      "field": "contract",
      "op": "changed",
      "from": "3333…",
      "to": "9999…",
      "tags": ["input", "security"],
      "detail": ["description or schema changed · effect=write"],
      "severity": true
    }
  ]
}
```
