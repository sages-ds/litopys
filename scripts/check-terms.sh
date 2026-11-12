#!/usr/bin/env bash
# Forbidden terms: names, domains, addresses and identifier patterns that must not appear in the public tree. The
# list itself is kept out of the repository — it would mention what it forbids — so it comes from the secret
# FORBIDDEN_TERMS_B64: base64 of a text file, one Perl regular expression per line, # for comments, matched
# case-insensitively.
#
# The report names the file, the line and the number of the pattern, never the term or the line's text.
#
#   scripts/check-terms.sh --tree <dir> [--warn]
#                                   every tracked file of the git tree at <dir> (the public repository: --tree .)
#   scripts/check-terms.sh [--warn] the maintainers' repository: the paths of scripts/public-allowlist.txt
#   --warn                          report, exit 0; without it any match is exit 1
#
# Without the secret the check is skipped, and says so.
set -euo pipefail

warn=0
tree=""
while [ $# -gt 0 ]; do
  case "$1" in
    --warn) warn=1; shift ;;
    --tree) tree="${2:?--tree needs a directory}"; shift 2 ;;
    *) echo "terms: unknown argument $1" >&2; exit 2 ;;
  esac
done

if [ -z "${FORBIDDEN_TERMS_B64:-}" ]; then
  echo "terms: FORBIDDEN_TERMS_B64 is not set — check skipped"
  exit 0
fi

if [ -n "$tree" ]; then
  cd "$tree"
  specs=(.)
else
  root=$(git rev-parse --show-toplevel)
  cd "$root"
  mapfile -t specs < <(grep -v -e '^#' -e '^[[:space:]]*$' scripts/public-allowlist.txt | tr -d '\r')
fi
# decode first, outside a process substitution: a broken secret must stop the check, not pass as an empty list
if ! list=$(printf '%s' "$FORBIDDEN_TERMS_B64" | base64 -d 2>/dev/null); then
  echo "terms: FORBIDDEN_TERMS_B64 is no base64" >&2
  exit 2
fi
mapfile -t patterns < <(printf '%s\n' "$list" | tr -d '\r')
if ! printf '%s\n' "${patterns[@]}" | grep -q -v -e '^#' -e '^[[:space:]]*$'; then
  echo "terms: the list has no pattern — the check would pass anything" >&2
  exit 2
fi

found=0
n=0
for p in "${patterns[@]}"; do
  case "$p" in '' | '#'*) continue ;; esac
  n=$((n + 1))
  # -I: no binary files; of each match only the file and the line number are kept
  rc=0
  # git's own error message quotes the pattern: it goes nowhere
  out=$(git grep -n -I -i -P -e "$p" -- "${specs[@]}" 2>/dev/null) || rc=$?
  if [ "$rc" -gt 1 ]; then # 1 is "no match"; more is a broken pattern or a git error — never a silent pass
    echo "terms: pattern #$n cannot be searched (git grep exit $rc)" >&2
    exit 2
  fi
  while IFS=: read -r file line _; do
    [ -n "$file" ] || continue
    echo "terms: pattern #$n — $file:$line"
    found=$((found + 1))
  done <<<"$out"
done

echo "terms: $n patterns, $found matches"
if [ "$found" -gt 0 ] && [ "$warn" -eq 0 ]; then
  exit 1
fi
