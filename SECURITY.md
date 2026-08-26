# Security

## Reporting a vulnerability

Please report privately through GitHub's [private vulnerability
reporting](https://github.com/waveoffai/waveoff/security/advisories/new) rather
than in a public issue.

Include what you did, what happened, and what you expected. A proof of concept
helps but is not required to report.

## What this project is responsible for

Waveoff sits on the path between an agent and the model and tool servers it
uses, and it decides whether a new version of that agent ships. Three things
follow, and they are where to look first:

**Cassettes must be safe to share.** Credentials are stripped at record time —
`Authorization`, `X-Api-Key`, cookies and a configurable regex set — before a
record exists, not before it is written. A cassette is meant to be safe to
commit to a repository and safe to hand to a vendor. A credential surviving into
one is a vulnerability.

**Unclassified tools must fail closed.** During replay and shadow, a tool with no
asserted `effect` is refused, never passed through. A server's own
`readOnlyHint` is a claim by the untrusted party in the tool-poisoning threat
model and is never promoted to an asserted effect. A path that lets an
unclassified tool execute is a vulnerability.

**Digest verification must not be bypassable.** The validating webhook
recomputes both digests and runs `failurePolicy: Fail`, so taking the webhook
down blocks manifest creation rather than admitting unverified ones. Spec
immutability is additionally enforced by CEL on the CRD, which holds even with
the webhook scaled to zero. A way to admit a manifest whose digests do not match
its spec is a vulnerability.

## Known limits, deliberately

These are documented rather than fixed, and are not vulnerabilities:

- **The manifest's integrity guarantee is uneven.** `code.image@sha256:…` is
  attested — the hash pins the bytes. Prompt, contract, policy and rubric
  digests are *asserted*: the content lives elsewhere and admission cannot check
  it. The honest claim is that this set of assertions has not changed, not that
  the referenced content is what it claims to be. See
  [docs/digest.md](docs/digest.md).
- **Shadow write suppression covers the MCP proxy only.** A pod-level
  NetworkPolicy cannot separate the agent container from the sidecar that shares
  its pod, so an allowed MCP server is reachable directly. See
  [docs/shadow.md](docs/shadow.md) for what that does and does not close.
- **A suppressed write always succeeds**, so shadow cannot see a candidate that
  regressed only in failure handling.

## The build and release pipeline

This project's whole argument is that you should know which bytes ran. It would
be a poor showing to make that argument and not hold the line on its own
pipeline, so:

- **Every GitHub Action is pinned to a commit SHA**, with the readable tag in a
  trailing `# ratchet:` comment. A tag can be moved by whoever controls the
  action's repository; a commit cannot.
- **Only first-party `actions/*` actions are used.** Every additional publisher
  is an additional account whose compromise becomes ours.
- **No `pull_request_target`.** It runs with a writable token and repository
  secrets on a pull request anybody can open, and it is the most common way a
  public repository is taken over. Pull requests build under `pull_request`,
  which gives a read-only token and no secrets.
- **No expression interpolation inside shell scripts.** A `${{ }}` expansion in
  a `run:` block is substituted before the shell sees it, so an attacker-shaped
  value becomes code. Values arrive through `env:` and are referenced as shell
  variables, where they are data.
- **Checkout does not persist credentials.** `actions/checkout` leaves the job
  token in `.git/config` by default, readable by every later step and by
  anything a build dependency drags in. No job here pushes, so no job needs it.
- **Least privilege.** No workflow-level `contents: write`; the release job asks
  for what it needs and every other job is read-only.
- **Release artefacts carry build provenance**, signed by GitHub's OIDC identity
  and recorded in a public transparency log:

  ```console
  gh attestation verify waveoff_v0.1.0_linux_amd64.tar.gz --repo waveoffai/waveoff
  ```

  The `SHA256SUMS` file proves an archive was not corrupted in transit. It
  proves nothing about who built it, and it is served from the same place as
  the archives — so anyone who can replace one can replace both. The
  attestation says which workflow at which commit produced those bytes.

`hack/check-pins.sh` enforces the first five on every push, and each check has
been verified by breaking it deliberately and watching the build fail.

### What is still trusted, and by whom

Being specific, because "supply chain secured" is not a thing anyone can
truthfully claim:

| Trusted | Why it is acceptable | What would improve it |
|---|---|---|
| GitHub, as the runner and the OIDC issuer | Unavoidable for a project hosted here | Nothing short of building elsewhere |
| The `actions/*` publisher, at the pinned commits | First-party, and a moved tag cannot reach us | Reviewing the pinned code before each bump |
| The Go module proxy and checksum database | `go.sum` and the public transparency log make a substituted dependency detectable | Vendoring |
| `cspell`, at an exact version, over pull-request content | Read-only token, no secrets; blast radius is one runner | Dropping it |
| cert-manager, Gateway API and Istio manifests, fetched by URL in `hack/e2e.sh` | Test-cluster setup only, in a read-only job with no secrets. Never on a user's cluster and never in a release | Pinning by content digest |
| Base images `golang` and `gcr.io/distroless/static` | Tag-pinned, not digest-pinned | Digest-pinning, at the cost of manual bumps |

The last two are known gaps rather than oversights. Neither can reach a release
artefact or a user's cluster; both would be worth closing before this project is
depended on by anyone who cannot tolerate that reasoning.

## No telemetry

Waveoff never phones home. There is no version check, no usage statistic, no
crash report — not behind a flag, not opt-out. `hack/check-boundary.sh` enforces
this on every push, so it is verifiable by reading one script rather than by
trusting a sentence in a README.
