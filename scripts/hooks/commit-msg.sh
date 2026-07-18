#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF' >&2
Usage:
  scripts/hooks/commit-msg.sh <commit-message-file>
  scripts/hooks/commit-msg.sh --message "<commit message>"
EOF
  exit 2
}

case "${1-}" in
  --message)
    [[ $# -eq 2 ]] || usage
    message="$2"
    ;;
  "")
    usage
    ;;
  *)
    [[ $# -eq 1 && -f "$1" ]] || usage
    message="$(sed '/^[[:space:]]*#/d' "$1")"
    ;;
esac

message="${message//$'\r'/}"
subject="$(printf '%s\n' "$message" | awk 'NF { print; exit }')"

if [[ -z "$subject" ]]; then
  echo "Commit message is empty." >&2
  exit 1
fi

type_pattern='feat|fix|deps|docs|chore|ci|refactor|perf|test|build|revert'
scope_pattern='(\([[:alnum:]./_-]+\))?'
header_pattern="^(${type_pattern})${scope_pattern}(!)?: .+"

if [[ "$subject" =~ ^(Merge|WIP|wip|fixup!|squash!) ]]; then
  echo "Commit message must use Conventional Commits, not generated merge/fixup text." >&2
  echo "Received: $subject" >&2
  exit 1
fi

if [[ ! "$subject" =~ $header_pattern ]]; then
  cat <<EOF >&2
Invalid Conventional Commit header:
  $subject

Expected format:
  <type>(optional-scope)!: description

Allowed types:
  feat, fix, deps, docs, chore, ci, refactor, perf, test, build, revert
EOF
  exit 1
fi

if printf '%s\n' "$message" | grep -Eq '^[[:space:]]*BREAKING[ -]CHANGE($|[^:])'; then
  echo "Use 'BREAKING CHANGE:' or 'BREAKING-CHANGE:' when declaring a breaking footer." >&2
  exit 1
fi
