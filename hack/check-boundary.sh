#!/usr/bin/env bash
#
# Enforces the rules that keep this module honest. Every one of these is a
# promise made in the README, and a promise a regulated buyer will verify by
# reading the source rather than by asking.
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
report() { echo; echo "FAIL: $1"; shift; printf '  %s\n' "$@"; fail=1; }

# 1. No feature gating.
#
# There is no 'pro' build tag, no enterprise/ directory, no stub that errors
# with an upgrade prompt, and no code path that behaves differently depending on
# a licence. Anyone reading this source sees everything it does.
hits=$(grep -rniE '(available in enterprise|upgrade to unlock|//go:build (pro|enterprise)|licen[cs]e[dK]ey)' \
        --include='*.go' --include='*.yaml' . || true)
if [[ -n "$hits" ]]; then
  report "found feature gating or an upsell string" "$hits"
fi

# 2. No telemetry, phone-home, or version check.
#
# Not anonymous usage statistics, not a version ping. Air-gapped regulated
# buyers read the source, and one outbound call you cannot justify costs that
# entire market.
#
# OpenTelemetry is explicitly not what this bans. It is instrumentation the
# operator configures and points wherever they choose; the rule is about
# outbound calls *we* make that they did not ask for.
hits=$(grep -rniE 'segment\.io|posthog|amplitude|mixpanel|sentry-go|datadoghq|phone.?home|checkForUpdate|usage.?stats' \
        --include='*.go' . \
        | grep -viE 'opentelemetry' || true)
if [[ -n "$hits" ]]; then
  report "found something that looks like telemetry" "$hits"
fi

# 3. The org handle is not the product name.
#
# 'waveoffai' exists only because the GitHub handle was taken. It is legal in
# an import path and nowhere else; suffixed brand names leak fast and are
# painful to walk back.
# The exclusions are repository and registry paths, where the handle is the
# address of a thing and not a name for the product. `waveoffai/waveoff` on its
# own is the form the gh CLI takes (`gh attestation verify --repo ...`).
hits=$(grep -rniE 'waveoffai' --include='*.go' --include='*.yaml' --include='*.md' . \
        | grep -v 'github.com/waveoffai/waveoff' \
        | grep -v 'ghcr.io/waveoffai/' \
        | grep -vE '(--repo|repos/)[ =]?waveoffai/waveoff' \
        | grep -v 'check-boundary.sh' || true)
if [[ -n "$hits" ]]; then
  report "the org handle leaked into a user-facing string; the product is Waveoff" "$hits"
fi

# 4. Every Go file carries a licence header.
missing=$(find . -name '*.go' -not -path './bin/*' -not -name 'zz_generated*' \
          -exec grep -L 'SPDX-License-Identifier: Apache-2.0' {} + || true)
if [[ -n "$missing" ]]; then
  report "these files have no licence header" "$missing"
fi

# 5. A failed gate is a wave-off.
#
# The vocabulary is load-bearing: the wave-off is the decision, the rollback is
# the implementation, and the API keeps the two words distinct.
hits=$(grep -rn --include='*.go' -iE '"(gate )?(failed|aborted) the (candidate|rollout)"' . || true)
if [[ -n "$hits" ]]; then
  report "a candidate is waved off, not failed or aborted" "$hits"
fi

if [[ $fail -eq 0 ]]; then
  echo "boundary checks passed"
fi
exit $fail
