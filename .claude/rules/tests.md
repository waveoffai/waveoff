# Which suite does this belong in

Pick the cheapest suite that could actually have caught the bug. Not the
cheapest suite that passes.

| Change | Suite |
|---|---|
| Canonicalisation, projection, statistics, diff rendering | `make test-unit` (add a property test if there is an invariant) |
| CRD schema, CEL, defaulting, SSA, anything the API server touches | `make test-envtest` |
| Recorder or replay against a real framework or real MCP server | `make test-integration` |
| Webhook wiring, injection, NetworkPolicy, traffic routers, install | `make test-e2e` |

## Rules

- **A fake client does not enforce CEL.** Anything that writes a CRD you do not
  own needs an envtest or e2e case, because the fake client will happily store
  an object a real API server rejects.
- **Establish the control before asserting the negative.** A test that asserts
  "the pod could not reach X" must first prove it *could* reach X without the
  policy. Otherwise it passes on a broken fixture and on a cluster whose CNI
  ignores NetworkPolicy entirely.
- **Fail loudly rather than skip** when a precondition is missing. A skipped
  safety test and a passing one look identical in a green run. `make test-e2e`
  fails, with an explanation, on a cluster that does not enforce NetworkPolicy.
- **Warm up fixtures outside the assertion.** Whether a reference server has
  finished starting is not what any case is about; letting that race decide an
  assertion makes the suite lie in both directions.
- **Name tests as sentences about behaviour**, not after the function under
  test: `TestAWriteTheIncumbentNeverMakesWithdrawsTheCandidate`, not
  `TestCompareActivity`.
- **Say why in the test comment.** What goes wrong in production if this
  regresses. A test whose comment restates its assertions has not earned its
  place.

## Test data

- Statistical tests use generated data with a known effect size, so the expected
  answer is known independently of the implementation.
- Golden files: `make test-golden` rewrites them. Read the diff — a golden file
  updated without being read is a test that has stopped testing.
