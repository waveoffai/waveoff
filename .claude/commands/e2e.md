# Work on the end-to-end suite

`test/e2e/` runs against a real kind cluster. It exists for what envtest
structurally cannot reach, and it is where this project's real bugs have been
found.

## Before writing a case

Read `.claude/rules/tests.md`. Then ask what a *fake* client would let through:
CEL rules, schema pruning, TLS wiring, CNI enforcement, container ordering,
process boundaries. If the answer is "nothing", the case belongs in a cheaper
suite.

## Structure

- `e2e_test.go` — `TestMain`, the per-run namespace, and the manifest helpers.
  Every run gets a fresh namespace, so fixtures use `NAMESPACE` and
  `MANIFEST_NAME` placeholders rather than fixed names.
- `inject_test.go` — sidecar injection on a live pod.
- `shadow_test.go` — write suppression, synthetic objects, egress confinement.
- `traffic_test.go` — both routers against real CRDs.
- `fixtures/` — YAML applied through `applyFixture`.

## Rules that have already been paid for

- **Prove the control first.** Assert the thing works *before* applying the
  policy that should stop it, or a broken fixture reads as a passing safety
  test.
- **Fail, do not skip,** when the cluster cannot support the assertion.
  `requireNetworkPolicyEnforcement` fails with an explanation rather than
  skipping, because a shadow stage on a non-enforcing cluster is not safe and a
  green suite must not say it is.
- **`kubectl exec` returns combined output.** Never substring-search it for a
  status code — a curl error message contains digits too. Fence the value.
- **Poll for content, not for existence.** A cassette exists as soon as its
  header is written; its spans arrive later, because the sink is asynchronous by
  design.
- **Drive the agent's traffic from inside the agent container.** Calling the
  sidecar from outside the pod proves the sidecar works and says nothing about
  whether the agent's traffic is routed through it.
- **Do a real protocol handshake.** The MCP reference server rejects a
  fabricated session id, and a suppressor that only ever sees calls the server
  would have refused proves nothing.

## Running

```
make test-e2e                    # create a cluster, run, tear down
KEEP_CLUSTER=1 make test-e2e     # leave it up
KEEP_NAMESPACE=1 go test -tags e2e -count=1 -v ./test/e2e/   # against a kept cluster
```

`hack/e2e.sh` installs cert-manager, the Gateway API CRDs (**experimental**
channel — `sessionPersistence` exists nowhere else) and the Istio CRDs.
