#!/bin/bash
set -uo pipefail
# Seed workspace from baked-in test repo if fresh (no .git yet)
if [ ! -d /workspace/.git ]; then
    cp -a /test-repo/. /workspace/
fi
# Hand off to the real entrypoint (skips clone when .git exists)
exec /entrypoint.sh "$@"
