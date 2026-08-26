# Verify a change

Run the suites in cost order and stop at the first failure. Report what actually
ran — a summary that says "tests pass" when the e2e suite was never started is
the failure mode this file exists to prevent.

## Steps

1. `make` — generate, fmt, vet, boundary, action pins, namespace check, unit and
   API-server tests, build.
   - If `make verify-generated` fails, run `make generate` and look at the diff
     before committing it. A regenerated `docs/digest.md` means the digest
     classification changed, which is a breaking change (see
     `.claude/rules/digests.md`).
2. `make lint-go` — golangci-lint. The selection is small on purpose; a new
   finding is usually real.
3. Decide from `.claude/rules/tests.md` whether this change needs
   `make test-integration` or `make test-e2e`. If it touches the recorder,
   replay, the webhooks, injection, or either traffic router, it does.
4. `make test-e2e` needs Docker and takes several minutes. `KEEP_CLUSTER=1`
   leaves the kind cluster up; `USE_EXISTING=1` runs against whatever kubectl
   points at.

## Reporting

State per suite: ran and passed, ran and failed (with the output), or did not
run (with why). If something is blocked, finish everything that is not and say
plainly what is left.
