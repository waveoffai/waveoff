#!/usr/bin/env bash
#
# Runs the end-to-end suite against a real cluster.
#
# envtest already covers the API server's behaviour. This exists for what
# envtest structurally cannot reach: the webhook served by a real Deployment
# behind a real Service, with a certificate issued by cert-manager and a CA
# bundle injected into the ValidatingWebhookConfiguration. That wiring is what
# actually breaks in an install, and it is invisible to every other test here.
#
#   ./hack/e2e.sh                 create a kind cluster, run, tear down
#   KEEP_CLUSTER=1 ./hack/e2e.sh  leave the cluster up to poke at
#   USE_EXISTING=1 ./hack/e2e.sh  run against whatever kubectl points at
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-waveoff-e2e}
IMG=${IMG:-waveoff-manager:e2e}
RECORDER_IMG=${RECORDER_IMG:-waveoff-recorder:e2e}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.19.1}
GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.4.0}
ISTIO_VERSION=${ISTIO_VERSION:-release-1.27}
MCP_IMAGE=${MCP_IMAGE:-waveoff-mcp-everything:e2e}
AGENT_IMAGE=${AGENT_IMAGE:-busybox:1.36}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
PROVIDER_IMAGE=${PROVIDER_IMAGE:-waveoff-fakeprovider:e2e}
KEEP_CLUSTER=${KEEP_CLUSTER:-}
USE_EXISTING=${USE_EXISTING:-}

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

if [[ -z "$USE_EXISTING" ]]; then
  if ! docker info >/dev/null 2>&1; then
    echo "Docker is not running. Start it, or point kubectl at a cluster and re-run with USE_EXISTING=1." >&2
    exit 1
  fi

  step "kind cluster: $CLUSTER"
  if go tool kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "reusing existing cluster"
  else
    go tool kind create cluster --name "$CLUSTER" --wait 120s
  fi
  export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
  go tool kind export kubeconfig --name "$CLUSTER"

  cleanup() {
    if [[ -z "$KEEP_CLUSTER" ]]; then
      step "tearing down $CLUSTER"
      go tool kind delete cluster --name "$CLUSTER" || true
    else
      echo
      echo "cluster kept: kubectl --context kind-$CLUSTER get agentmanifests -A"
    fi
  }
  trap cleanup EXIT
fi

step "cert-manager $CERT_MANAGER_VERSION"
# The webhook's serving certificate is issued by cert-manager, and its
# ca-injector is what puts the CA bundle into the webhook configuration.
# Without both, the API server cannot verify the webhook and — under
# failurePolicy: Fail — rejects every AgentManifest in the cluster.
if kubectl get deployment -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
  echo "already installed"
else
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
fi
kubectl wait --for=condition=Available --timeout=300s \
  -n cert-manager deployment/cert-manager deployment/cert-manager-webhook deployment/cert-manager-cainjector

step "Gateway API CRDs ($GATEWAY_API_VERSION, experimental channel)"
# The experimental channel, not the standard one. sessionPersistence — the
# field a live stage's session-affinity check reads — exists only there, and on
# a standard-channel install the API server prunes it silently: the apply
# succeeds and the field is simply gone. test/e2e/traffic_test.go asserts both
# behaviours, so this install decides which half runs.
kubectl apply --server-side --force-conflicts \
  -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml"

step "Istio CRDs ($ISTIO_VERSION)"
# CRDs only. The Istio router works through unstructured objects to keep a very
# large dependency tree out of this module, and the cost of that is that nothing
# checks field names at compile time — so they have to be checked against the
# real schema. No control plane is needed for that.
kubectl apply --server-side --force-conflicts \
  -f "https://raw.githubusercontent.com/istio/istio/${ISTIO_VERSION}/manifests/charts/base/files/crd-all.gen.yaml"

# load_image copies a local image onto the kind node.
#
# It goes via `docker save --platform` rather than `kind load docker-image`,
# because a multi-platform image in the local store carries an index and
# attestation manifests that kind cannot import: it fails with "content digest
# ... not found". Pinning the platform on save flattens it to the one the node
# can actually run.
load_image() {
  local image=$1 archive
  archive=$(mktemp -t waveoff-img-XXXXXX).tar
  docker save --platform "linux/$(go env GOARCH)" "$image" -o "$archive"
  go tool kind load image-archive "$archive" --name "$CLUSTER"
  rm -f "$archive"
}

step "build and load $IMG and $RECORDER_IMG"
docker build --build-arg TARGET=manager -t "$IMG" .
docker build --build-arg TARGET=waveoff-recorder -t "$RECORDER_IMG" .
if [[ -z "$USE_EXISTING" ]]; then
  load_image "$IMG"
  load_image "$RECORDER_IMG"
  # The pin case introspects a real MCP server rather than a fake one written
  # to our own reading of the protocol. Preload it and the agent image so the
  # test does not depend on the node pulling at the moment it runs, and so a
  # rate-limited CI runner does not fail on a registry quota.
  # A current reference MCP server, built rather than pulled: see the comment
  # in fixtures/mcp-server.Dockerfile.
  docker build -f test/e2e/fixtures/mcp-server.Dockerfile -t "$MCP_IMAGE" test/e2e/fixtures
  load_image "$MCP_IMAGE"
  docker pull --platform "linux/$(go env GOARCH)" "$AGENT_IMAGE"
  load_image "$AGENT_IMAGE"
  docker pull --platform "linux/$(go env GOARCH)" "$CURL_IMAGE"
  load_image "$CURL_IMAGE"
  # A fake model provider, built from this repository, so the injection test
  # can prove the sidecar reaches a real upstream without an API key or egress.
  docker build -f test/fixtures/fakeprovider.Dockerfile -t "$PROVIDER_IMAGE" .
  load_image "$PROVIDER_IMAGE"
fi

step "deploy"
work=$(mktemp -d)
trap 'rm -rf "$work"' RETURN
cp -R config "$work/config"
python3 - "$work/config/default/kustomization.yaml" "$IMG" <<'PY'
import sys
path, img = sys.argv[1], sys.argv[2]
name, _, tag = img.rpartition(":")
s = open(path).read()
s = s.replace("    newName: ghcr.io/waveoffai/waveoff-manager", "    newName: " + name)
s = s.replace("    newTag: latest", "    newTag: " + tag)
open(path, "w").write(s)
PY

# The image the manager injects is an argument, not an image: field, so
# kustomize's image transformer cannot reach it. Without this the sidecar is
# injected correctly and then fails to pull.
sed -i.bak "s|value: ghcr.io/waveoffai/waveoff-recorder:latest|value: ${RECORDER_IMG}|" \
  "$work/config/manager/manager.yaml"
rm -f "$work/config/manager/manager.yaml.bak"

kubectl apply -k "$work/config/default"

step "wait for the webhook to be serving"
kubectl wait --for=condition=Available --timeout=180s -n waveoff-system deployment/waveoff-manager
# Available is not the same as "the CA bundle has landed". Poll for it, because
# every assertion downstream depends on it and the failure is otherwise a wall
# of TLS errors.
for _ in $(seq 1 60); do
  bundle=$(kubectl get validatingwebhookconfiguration validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)
  [[ -n "$bundle" ]] && break
  sleep 2
done
if [[ -z "${bundle:-}" ]]; then
  echo "cert-manager never injected a CA bundle into the webhook configuration." >&2
  kubectl describe certificate -n waveoff-system waveoff-webhook-cert >&2 || true
  exit 1
fi
echo "CA bundle injected"

step "build the CLI"
go build -o bin/waveoff ./cmd/waveoff

step "run the suite"
WAVEOFF_BIN="$PWD/bin/waveoff" go test -tags e2e -count=1 -v -timeout 15m ./test/e2e/
