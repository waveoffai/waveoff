# Releasing

## What a version means here

Semantic versioning, with one addition: **a digest that changes meaning is a
breaking change even when the schema does not.** Moving a field in or out of
`behaviorDigest` gives every manifest already issued a new identity, and
somebody's approved release artefact stops verifying. That is a major-version
event with a migration note, not a footnote.

While the API is `v1alpha1`, breaking CRD changes may land in a minor release —
but never without a conversion plan and an entry in `CHANGELOG.md`.

## Before tagging

- [ ] `make` green on the commit being tagged
- [ ] `make lint-go` clean
- [ ] `make test-integration` green
- [ ] `make test-e2e` green
- [ ] `CHANGELOG.md` has an entry under the new version, not under `Unreleased`
- [ ] Any digest-classification change is marked breaking, with the remediation
      command (`waveoff verify --write`) spelled out
- [ ] The README status table still describes reality — in particular, which
      layers are shipped but **not yet validated against production traffic**

That last one matters more than it looks. Overstating maturity is how somebody
lets an unvalidated gate roll back production unattended.

## Cutting it

```console
# 1. Move Unreleased under the new version, dated. Leave a fresh Unreleased.
$EDITOR CHANGELOG.md
git commit -am "release: vX.Y.Z"

# 2. Tag and push.
git tag vX.Y.Z
git push origin main vX.Y.Z
```

`.github/workflows/release.yaml` then:

1. re-runs the boundary, namespace, unit and sample checks — a release must be
   built from a tree that passes its own checks;
2. builds `waveoff` for darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 and
   windows/amd64, with the version and commit compiled in;
3. writes `SHA256SUMS`. Not decoration: air-gapped and regulated users are the
   ones most likely to run this, and they verify downloads;
4. renders `waveoff-<version>.yaml` and `waveoff-crds-<version>.yaml`, so
   `kubectl apply -f <url>` works without cloning or having kustomize;
5. creates the GitHub release with generated notes.

## A dry run

```console
gh workflow run release.yaml -f tag=vX.Y.Z-rc1
```

Builds and uploads artefacts without publishing a release.

## After

- Download one archive, check it against `SHA256SUMS`, run `waveoff version`.
- Apply the rendered manifest to a throwaway cluster and confirm the webhook
  comes up: `make quickstart` covers the same ground.
