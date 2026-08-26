# Cut a release

Read `RELEASING.md` first; this is the agent-facing summary of it.

## Preconditions

- `make` green, `make lint-go` clean, `make test-e2e` green on the commit being
  tagged. A release built from a tree that has not passed its own checks is
  worse than a late one.
- `CHANGELOG.md` has an entry under the version being cut, not under
  `Unreleased`.
- Any digest-classification change in this release is flagged as breaking, with
  the remediation command spelled out.

## Steps

1. Move the `Unreleased` section of `CHANGELOG.md` under the new version with
   today's date. Leave a fresh empty `Unreleased`.
2. Commit. Tag `vX.Y.Z`. Push the tag.
3. `.github/workflows/release.yaml` builds the five platform archives, the
   SHA256SUMS file, and the two rendered install manifests, then creates the
   GitHub release.
4. Verify the published artefacts: download one archive, check its checksum,
   run `waveoff version`.

## What a release must not do

- Change the meaning of a digest without saying so. That invalidates every
  manifest already issued, and someone's approved release artefact stops
  verifying.
- Ship a CRD change with no conversion plan.
