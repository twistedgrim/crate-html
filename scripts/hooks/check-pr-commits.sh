#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: scripts/hooks/check-pr-commits.sh <base-ref> <head-ref>" >&2
  exit 2
fi

base_ref="$1"
head_ref="$2"
merge_base="$(git merge-base "$base_ref" "$head_ref")"

while IFS= read -r commit; do
  [[ -n "$commit" ]] || continue
  message="$(git show -s --format=%B "$commit")"
  subject="$(git show -s --format=%s "$commit")"
  echo "Validating $commit $subject"
  bash scripts/hooks/commit-msg.sh --message "$message"
done < <(git rev-list --reverse "${merge_base}..${head_ref}")
