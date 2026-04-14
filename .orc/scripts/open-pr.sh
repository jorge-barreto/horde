#!/usr/bin/env bash
set -euo pipefail
cd "$PROJECT_ROOT"

BRANCH="horde/${TICKET}"

# Final push to make sure everything is on remote
git push origin "HEAD:refs/heads/${BRANCH}" --force-with-lease 2>&1 || {
    git push origin "HEAD:refs/heads/${BRANCH}" --force 2>&1
}

# Repo in owner/repo format (strip .git suffix and host prefix)
REPO_SLUG=$(git remote get-url origin | sed -E 's#https?://[^/]+/##; s#\.git$##')

# Skip if a PR already exists for this branch
if gh pr view "$BRANCH" --repo "$REPO_SLUG" &>/dev/null; then
    echo "PR already exists for $BRANCH"
    exit 0
fi

# Build PR body from commit log
COMMITS=$(git log --oneline origin/main..HEAD)
BODY="## Summary

Automated implementation for \`${TICKET}\`.

## Commits

\`\`\`
${COMMITS}
\`\`\`
"

gh pr create \
    --repo "$REPO_SLUG" \
    --head "$BRANCH" \
    --base main \
    --title "${TICKET}" \
    --body "$BODY"

echo "PR created for $BRANCH"
