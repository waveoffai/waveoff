# Changing the digest

`behaviorDigest` is the identity of an agent version. Moving a field into or out
of it changes the identity of **every manifest already issued** — a breaking
change even when the CRD schema is untouched, because it silently invalidates
objects that are already in git and already approved.

## The rules

- The classification map in `internal/digest/classify.go` is normative. It is
  the only place a field's treatment is decided.
- **When in doubt, include.** Including a behaviourally irrelevant field costs
  one unnecessary canary. Excluding a relevant one ships an unvalidated change.
- The map must be **exhaustive** over `AgentManifestSpec`. A reflection-walk
  test fails if any field path is missing, so a new CRD field cannot land
  outside the hash by accident.
- The projection is built **field by field**, never by marshalling the struct.
  Struct tags are not part of the digest contract.
- Absent versus zero is decided per field by `NullSemantics`, not by JCS.
  `temperature` unset and `temperature: 0` must hash differently — one is a
  provider default, the other is greedy decoding.

## When you change it

1. Update `classify.go`.
2. `make generate` — `docs/digest.md` is generated from the map, so a rationale
   that has drifted from the code is impossible rather than merely discouraged.
3. Add the change to `CHANGELOG.md` under a heading that says it is breaking.
4. Say what an operator has to run: `waveoff verify --write` on every manifest.

## What the digests do and do not attest

Be precise about this; a compliance reader will ask.

- **Attested**: `code.image@sha256:…`. Self-verifying — the hash pins the bytes.
- **Asserted**: `prompts[].digest`, `tools[].contractDigest`,
  `policy.bundleDigest`, `retrieval.indexSnapshot`, `judges[].rubricDigest`.
  The referenced content lives elsewhere and admission cannot check it.

The honest claim is "these assertions have not changed", not "the referenced
content is what it claims to be". Do not blur the two.
