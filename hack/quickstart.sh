#!/usr/bin/env bash
#
# Waveoff on a throwaway cluster, in one command.
#
# Brings up kind, installs cert-manager and Waveoff, then walks the loop an
# operator actually runs: pin a Deployment into a manifest, seal it, apply it,
# change it, and diff the two versions to see what a rollback would have to
# undo.
#
# It is a demonstration, not an installer. It creates a cluster and deletes it
# again unless you ask otherwise.
#
#   ./hack/quickstart.sh                 create, demonstrate, tear down
#   KEEP_CLUSTER=1 ./hack/quickstart.sh  leave the cluster up
#   USE_EXISTING=1 ./hack/quickstart.sh  use whatever kubectl points at
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-waveoff-quickstart}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.19.1}
IMG=${IMG:-waveoff-manager:quickstart}
KEEP_CLUSTER=${KEEP_CLUSTER:-}
USE_EXISTING=${USE_EXISTING:-}
NS=${NS:-demo}

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
note() { printf '\033[2m%s\033[0m\n' "$1"; }

if [[ -z "$USE_EXISTING" ]]; then
  docker info >/dev/null 2>&1 || {
    echo "Docker is not running. Start it, or point kubectl at a cluster and re-run with USE_EXISTING=1." >&2
    exit 1
  }
  step "kind cluster: $CLUSTER"
  if go tool kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "reusing existing cluster"
  else
    go tool kind create cluster --name "$CLUSTER" --wait 120s
  fi
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

step "cert-manager"
# The webhook's serving certificate comes from cert-manager, and its ca-injector
# puts the CA bundle into the webhook configuration. Without both, the API
# server cannot verify the webhook — and under failurePolicy: Fail that means
# every AgentManifest in the cluster is rejected.
if kubectl get deployment -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
  echo "already installed"
else
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
fi
kubectl wait --for=condition=Available --timeout=300s \
  -n cert-manager deployment/cert-manager deployment/cert-manager-webhook deployment/cert-manager-cainjector

step "build and install Waveoff"
make build
docker build --quiet --build-arg TARGET=manager -t "$IMG" . >/dev/null
if [[ -z "$USE_EXISTING" ]]; then
  archive=$(mktemp -t waveoff-img-XXXXXX).tar
  # Via `docker save --platform` rather than `kind load docker-image`: a
  # multi-platform image in the local store carries an index kind cannot import.
  docker save --platform "linux/$(go env GOARCH)" "$IMG" -o "$archive"
  go tool kind load image-archive "$archive" --name "$CLUSTER"
  rm -f "$archive"
fi

work=$(mktemp -d)
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
kubectl apply -k "$work/config/default"
rm -rf "$work"

kubectl wait --for=condition=Available --timeout=180s -n waveoff-system deployment/waveoff-manager
note "waiting for cert-manager to inject the CA bundle"
for _ in $(seq 1 60); do
  bundle=$(kubectl get validatingwebhookconfiguration validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)
  [[ -n "$bundle" ]] && break
  sleep 2
done
[[ -n "${bundle:-}" ]] || { echo "the CA bundle never landed; the webhook cannot serve" >&2; exit 1; }

step "an agent to pin"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: support-agent
  labels: {app: support-agent}
spec:
  replicas: 1
  selector:
    matchLabels: {app: support-agent}
  template:
    metadata:
      labels: {app: support-agent}
    spec:
      containers:
        - name: agent
          # A mutable tag on purpose: `waveoff pin` resolves the digest that is
          # actually running, from the pod status, not the one the spec claims.
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env:
            - name: ANTHROPIC_MODEL
              value: claude-sonnet-4-6
YAML
kubectl -n "$NS" rollout status deployment/support-agent --timeout=120s

step "waveoff pin"
note "introspects the running Deployment and writes a manifest, with a TODO on"
note "everything it refuses to guess — tool effects above all, because an"
note "unclassified tool fails closed rather than being passed through."
./bin/waveoff pin "deployment/support-agent" -n "$NS" --agent support-agent -o /tmp/waveoff-demo.yaml || true
sed -n '1,40p' /tmp/waveoff-demo.yaml

step "the manifest is rejected until it is sealed"
note "digests are authored, not computed at admission: nothing rewrites what"
note "you applied, so the digest an auditor reads was committed to git."
kubectl apply -n "$NS" -f /tmp/waveoff-demo.yaml 2>&1 | head -20 || true

cat > /tmp/waveoff-a.yaml <<YAML
apiVersion: waveoff.ai/v1alpha1
kind: AgentManifest
metadata:
  name: PLACEHOLDER
  namespace: $NS
spec:
  agent: support-agent
  behaviorDigest: ""
  contentDigest: ""
  code:
    image: registry.internal/support-agent@sha256:1111111111111111111111111111111111111111111111111111111111111111
  model:
    provider: anthropic
    id: claude-sonnet-4-6
    pin: "2026-05-01"
    params:
      temperature: 0.2
  tools:
    - name: docs.search
      server: https://docs-gw.internal/mcp
      contractDigest: sha256:2222222222222222222222222222222222222222222222222222222222222222
      effect: read
      replayPolicy: snapshot
YAML
sed 's/temperature: 0.2/temperature: 0.7/' /tmp/waveoff-a.yaml > /tmp/waveoff-b.yaml

step "waveoff verify --write"
./bin/waveoff verify --write /tmp/waveoff-a.yaml
./bin/waveoff verify --write /tmp/waveoff-b.yaml
kubectl apply -f /tmp/waveoff-a.yaml
kubectl apply -f /tmp/waveoff-b.yaml
kubectl get agentmanifests -n "$NS"

step "waveoff diff"
note "the output an on-call engineer reads: which planes changed, what reaches"
note "the model, and what changes the gate itself."
./bin/waveoff diff /tmp/waveoff-a.yaml /tmp/waveoff-b.yaml || true

step "the spec is immutable"
note "CEL on the CRD, so it holds even with the webhook scaled to zero."
kubectl patch agentmanifest -n "$NS" "$(./bin/waveoff verify /tmp/waveoff-a.yaml >/dev/null && grep '^  name:' /tmp/waveoff-a.yaml | awk '{print $2}')" \
  --type=merge -p '{"spec":{"model":{"id":"something-else"}}}' 2>&1 | head -5 || true

echo
step "done"
echo "Next: docs/digest.md for what the digests cover, docs/gating.md for the gate,"
echo "docs/shadow.md for what a shadow stage does and does not guarantee."
