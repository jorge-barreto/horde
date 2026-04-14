#!/usr/bin/env bash
set -euo pipefail
cd "$PROJECT_ROOT"

BRANCH="horde/${TICKET}"
git push origin "HEAD:refs/heads/${BRANCH}" --force-with-lease 2>&1 || {
    git push origin "HEAD:refs/heads/${BRANCH}" --force 2>&1
}
echo "Pushed to branch $BRANCH"
