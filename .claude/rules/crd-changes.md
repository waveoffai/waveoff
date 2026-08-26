# CRD changes

## Before you add a field

- Does it determine behaviour? Then it belongs in `behaviorDigest` — see
  `digests.md`, and expect the exhaustiveness test to fail until you classify it.
- Can it be validated by CEL on the CRD rather than only in the webhook? Prefer
  CEL: it keeps holding when the webhook is down.
- Is it a set keyed by name? Then names must be unique and the *projection*
  sorts them. **Never reorder the stored object** — that is a mutating webhook
  by another name and it breaks GitOps.

## CEL

- Root-level `x-kubernetes-validations` can reference only `metadata.name` and
  `metadata.generateName`. Referencing `metadata.annotations` is a compile error
  and the API server refuses to install the CRD. This was verified empirically;
  it is why the evidence-annotation freeze lives only in the webhook.
- Write the rejection message for the person reading it at 2am. Name the command
  that fixes it. A message that says "invalid" has wasted the one chance you had
  to help.
- A typeless field (`temperature: {}`) needs
  `controller-gen "crd:allowDangerousTypes=true"`.

## After you change it

```
make generate      # deepcopy, CRDs, webhook config, docs
make test-envtest  # the only suite that runs your CEL against a real API server
```

`make verify-generated` fails CI if you skipped step one.

## Traps this repository has already hit

- **A field can exist in Go and be pruned by the API server.** Gateway API's
  `sessionPersistence` lives in the experimental channel only; on a
  standard-channel install the write succeeds and the field is silently gone.
  Anything you read back from a CRD you do not own needs an e2e case.
- **CEL rules the fake client does not enforce.** `RequestMirror` requires a
  port on a Service reference. Unit tests against a fake client passed for
  months while no real cluster would have accepted the object.
- **`corpus` optional with `corpus.ref` `MinLength=1`** made a live stage
  impossible to create. Check the interaction between optional parents and
  required children.
