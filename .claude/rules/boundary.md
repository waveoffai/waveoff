# The licensing and telemetry boundary

Enforced by `hack/check-boundary.sh` on every push, because these are promises
made in the README and a regulated reader verifies them by reading the source.

## Never

- **Telemetry of any kind.** No phone-home, no version-check ping, no anonymous
  usage statistics, no crash reporting. Not behind a flag, not opt-out, not
  opt-in. The grep gate bans known SDKs and outbound-call helpers.
  ("OpenTelemetry" is exempt — it exports where the *operator* configured, and
  is off unless they ask.)
- **Metering or gating** of recording, replay or corpus size.
- **"Enterprise", "upgrade to unlock"** or similar in user-facing output.
- **The GitHub organisation handle** from the module path, in a user-facing
  string. It exists only because the plain name was taken, and it is legal in an
  import path and nowhere else. The check has no file exemptions — which is why
  this line names it indirectly rather than spelling it out.

## Workflow rules

`hack/check-pins.sh` also refuses, on every push:

- an Action pinned to a tag rather than a commit SHA (`make update-pins` fixes
  it, and the resulting diff is a supply-chain change meant to be reviewed as
  one);
- a `${{ }}` expression inside a `run:` block — it is substituted before the
  shell sees it, so an attacker-shaped value becomes code. Bind it in `env:`;
- `pull_request_target`, which runs untrusted code with a writable token and
  secrets;
- `actions/checkout` without `persist-credentials: false`, which otherwise
  leaves the job token in `.git/config` for every later step to read.

See SECURITY.md for what is still trusted and by whom.

## Also enforced

- Apache-2.0 licence headers on every source file.
- The wave-off vocabulary: the decision is a wave-off, the implementation is a
  rollback.

## Why it is a build failure and not a review convention

Because a reviewer who is tired agrees with the author, and this is exactly the
kind of thing that arrives as one harmless line in a large PR. The check costs
a second and removes the conversation entirely.
