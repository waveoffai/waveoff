## What this changes

<!-- What behaviour is different afterwards. Not a restatement of the diff. -->

## Why

<!-- What goes wrong without it. If this is a bug fix, what the bug did. -->

## How it was verified

<!-- Which suite, and what it proves. "Tests pass" is not an answer; the
     interesting cases in this repository were all found by running against
     real infrastructure, so say which. -->

- [ ] `make` (unit, property, API-server)
- [ ] `make test-integration` (real LangGraph agent, real MCP server)
- [ ] `make test-e2e` (real cluster)
- [ ] Not verifiable by any of the above — explained below

## Checklist

- [ ] The digest classification map is unchanged, or `docs/digest.md` was
      regenerated and the change is described in CHANGELOG.md.
      <!-- A field moving in or out of behaviorDigest changes the identity of
           every manifest already issued. It is a breaking change even when the
           schema is untouched. -->
- [ ] No new telemetry, phone-home, version check or usage statistic.
      <!-- `make lint-boundary` enforces this; it is a promise in the README. -->
- [ ] Any new tool effect fails closed.
- [ ] User-facing strings say "wave off" for the decision and "roll back" for
      the implementation.
- [ ] A limitation this introduces is written down somewhere a user will find
      it, not only in the PR.
