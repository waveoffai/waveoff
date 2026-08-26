#!/usr/bin/env bash
#
# Re-resolves every `# ratchet:<owner>/<repo>@<tag>` comment to that tag's
# current commit SHA and rewrites the pin in front of it.
#
# Run it deliberately — an update to a pinned action is a supply-chain change,
# and the diff it produces is the thing a reviewer is meant to look at. It is
# not run in CI: a job that silently moves its own pins is a job that pins
# nothing.
#
#   ./hack/update-pins.sh
set -euo pipefail
cd "$(dirname "$0")/.."

command -v gh >/dev/null || { echo "gh is required to resolve tags" >&2; exit 1; }

resolve() {
  local repo=$1 tag=$2 sha type
  sha=$(gh api "repos/${repo}/git/ref/tags/${tag}" --jq '.object.sha')
  type=$(gh api "repos/${repo}/git/ref/tags/${tag}" --jq '.object.type')
  # An annotated tag points at a tag object, which points at the commit.
  if [[ "$type" == "tag" ]]; then
    sha=$(gh api "repos/${repo}/git/tags/${sha}" --jq '.object.sha')
  fi
  printf '%s' "$sha"
}

for file in .github/workflows/*.y*ml; do
  while IFS= read -r spec; do
    repo=${spec%@*}
    tag=${spec#*@}
    sha=$(resolve "$repo" "$tag")
    echo "$repo@$tag -> $sha"
    # Replace whatever is currently pinned in front of this ratchet comment.
    python3 - "$file" "$repo" "$tag" "$sha" <<'PY'
import re, sys
path, repo, tag, sha = sys.argv[1:5]
s = open(path).read()
pattern = re.compile(r'uses:\s*' + re.escape(repo) + r'@\S+\s*# ratchet:' + re.escape(repo) + r'@' + re.escape(tag))
s = pattern.sub(f'uses: {repo}@{sha} # ratchet:{repo}@{tag}', s)
open(path, 'w').write(s)
PY
  done < <(grep -oh '# ratchet:[^ ]*' "$file" 2>/dev/null | sed 's/# ratchet://' | sort -u)
done

./hack/check-pins.sh
