#!/usr/bin/env bash
#
# Tick the rebase checkbox in all open Renovate PRs in a GitHub repo so
# Renovate rebases them onto the latest default branch on its next run.
#
# Usage:
#   scripts/renovate-rebase-tick.sh                    # defaults to origin
#   scripts/renovate-rebase-tick.sh yelinaung/trackkr  # explicit repo
#
# Requires the `gh` CLI to be authenticated.
#
set -euo pipefail

REPO="${1:-}"
if [ -z "$REPO" ]; then
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null)" || {
    echo "usage: $0 [<owner/repo>]" >&2
    exit 1
  }
fi

# Fetch open PRs authored by the Renovate GitHub App.
prs_json="$(gh pr list \
  --repo "$REPO" \
  --author app/renovate \
  --state open \
  --json number,title,url)"

echo "$prs_json" | jq -c '.[]' | while IFS= read -r pr; do
  number="$(printf '%s' "$pr" | jq -r .number)"
  title="$(printf '%s' "$pr" | jq -r .title)"
  url="$(printf '%s' "$pr" | jq -r .url)"

  body="$(gh pr view "$number" --repo "$REPO" --json body -q .body)"

  # Skip if already ticked.
  if printf '%s' "$body" | grep -q -- '- \[x\] <!-- rebase-check -->'; then
    echo "PR #${number} already ticked — skipping  ($title)"
    continue
  fi

  # Match both checked/unchecked variants to be safe.
  new_body="$(printf '%s' "$body" | sed 's/- \[ \] <!-- rebase-check -->/- [x] <!-- rebase-check -->/')"

  if [ "$new_body" = "$body" ]; then
    echo "PR #${number} no rebase checkbox found — skipping  ($title)"
    continue
  fi

  gh pr edit "$number" --repo "$REPO" --body "$new_body" >/dev/null
  echo "PR #${number} rebase checkbox ticked  ($title)"
  echo "  $url"
done