# Review a change

Review against the invariants, not against taste.

## Read first

- `CLAUDE.md` — the invariants
- The rule file for the area touched, in `.claude/rules/`

## Check, in order

1. **Does it fail open anywhere?** An error path that returns `nil`, a default
   that promotes, an unclassified tool that passes through, a missing verdict
   read as a pass. This is the class of bug that ships a regression, and it is
   worth more attention than everything below combined.
2. **Does it change a digest's meaning?** If `internal/digest/classify.go`
   moved, every manifest already issued has a new identity. Breaking, changelog,
   remediation command.
3. **Does it weaken a stated guarantee?** Suppression, credential redaction,
   `failurePolicy: Fail`, the no-telemetry rule. If it does, is the weakening
   documented where a user will find it, or only in the PR?
4. **Could the test have caught the bug?** A test that exercises the fixed path
   and would also pass on the broken one is not a regression test. Ask which
   suite it is in and whether that suite can see the failure at all — a fake
   client cannot see a CEL rejection.
5. **Do the error messages help?** A rejection an operator reads at 2am should
   name the command that fixes it.
6. **Is a new limitation written down?** This project's convention is to assert
   limitations in tests so they cannot be quietly forgotten.

## Reporting

Lead with anything in category 1. Separate "this is wrong" from "I would have
done it differently" and say which you mean. If you are unsure whether something
is a bug, say what input would make it one.
