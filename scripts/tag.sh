#!/usr/bin/env bash
#
# Tag every Go module in this repository whose code changed since its last tag.
#
# CI runs this on main after the tests pass. Each module has its own tag
# series: v* for the root, native/v* and native/provider/<name>/v* for the
# nested modules. A change counts when a non-test .go file, go.mod, or go.sum
# of the module changed. Nested modules do not count for their parent.
#
# The next version is the patch bump of the last tag. To pick another version,
# add its heading to CHANGELOG.md, for example "## [v0.4.0]" or
# "## [native/v0.3.0]". The highest untagged heading above the last tag wins.
#
# Usage:
#   scripts/tag.sh            # create and push the tags
#   DRY_RUN=1 scripts/tag.sh  # print the tags only
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# The last field is a space-separated list of nested module dirs to skip.
modules=(
  ".|v|native/"
  "native|native/v|native/provider/"
  "native/provider/anthropic|native/provider/anthropic/v|"
  "native/provider/openai|native/provider/openai/v|"
)

# Prints the highest of the given versions (without the "v").
highest() { printf '%s\n' "$@" | sort -V | tail -1; }

pushed=()
for m in "${modules[@]}"; do
  IFS='|' read -r dir prefix skip <<<"$m"

  last=$(git tag -l "${prefix}[0-9]*" | sed "s|^${prefix}||" | sort -V | tail -1)
  if [[ -z "$last" ]]; then
    echo "${dir}: no ${prefix}* tag yet, tag the first version by hand"
    continue
  fi

  path="$dir"
  [[ "$dir" == "." ]] && path=""
  changed=$(git diff --name-only "${prefix}${last}" HEAD -- "${path:-.}" |
    grep -E '(\.go|/?go\.mod|/?go\.sum)$' |
    grep -vE '_test\.go$|(^|/)testdata/' || true)
  for s in $skip; do
    changed=$(grep -v "^${s}" <<<"$changed" || true)
  done
  if [[ -z "$changed" ]]; then
    echo "${dir}: no code change since ${prefix}${last}"
    continue
  fi

  IFS=. read -r major minor patch <<<"$last"
  next="${major}.${minor}.$((patch + 1))"
  while read -r v; do
    [[ -z "$v" ]] && continue
    git rev-parse -q --verify "refs/tags/${prefix}${v}" >/dev/null && continue
    [[ "$(highest "$v" "$last")" == "$last" ]] && continue
    next=$(highest "$next" "$v")
  done < <(grep -oE "^## \[${prefix}[0-9]+\.[0-9]+\.[0-9]+\]" CHANGELOG.md |
    sed -E "s|^## \[${prefix}||; s|\]$||")

  tag="${prefix}${next}"
  echo "${dir}: ${prefix}${last} -> ${tag}"
  if [[ -z "${DRY_RUN:-}" ]]; then
    git tag "$tag" HEAD
    pushed+=("refs/tags/${tag}")
  fi
done

if (( ${#pushed[@]} )); then
  git push origin "${pushed[@]}"
fi
