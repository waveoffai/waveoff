#!/usr/bin/env bash
#
# Proves the deployment namespace is really configurable.
#
# Two references to it are strings embedded inside other values — the
# certificate's SANs and cert-manager's inject-ca-from annotation — and
# kustomize's namespace transformer does not reach into either. If they are
# ever left behind, the webhook cannot complete a TLS handshake, and because it
# runs failurePolicy: Fail that rejects every AgentManifest in the cluster.
#
# The failure is silent at render time and total at runtime, so it is checked
# here rather than discovered in an install.
set -euo pipefail
cd "$(dirname "$0")/.."

readonly DEFAULT_NS=waveoff-system
readonly TEST_NS=waveoff-nscheck

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -R config "$work/config"

sed -i.bak "s|^namespace: ${DEFAULT_NS}$|namespace: ${TEST_NS}|" "$work/config/default/kustomization.yaml"
rm -f "$work/config/default/kustomization.yaml.bak"

rendered=$(kubectl kustomize "$work/config/default")

if grep -q "$DEFAULT_NS" <<<"$rendered"; then
  echo "FAIL: '$DEFAULT_NS' survives a namespace change:"
  grep -n "$DEFAULT_NS" <<<"$rendered" | sed 's/^/  /'
  echo
  echo "Every reference must derive from the namespace field in"
  echo "config/default/kustomization.yaml. Add a kustomize replacement for the"
  echo "ones above; a plain namespace transformer cannot reach a string that is"
  echo "embedded inside another value."
  exit 1
fi

# And the derived values must be right, not merely free of the old name.
for want in \
  "waveoff-webhook.${TEST_NS}.svc" \
  "waveoff-webhook.${TEST_NS}.svc.cluster.local" \
  "cert-manager.io/inject-ca-from: ${TEST_NS}/waveoff-webhook-cert"
do
  if ! grep -qF "$want" <<<"$rendered"; then
    echo "FAIL: expected '$want' in the rendered output"
    exit 1
  fi
done

echo "namespace is configurable: all references follow"
