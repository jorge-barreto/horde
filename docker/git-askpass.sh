#!/bin/bash
# GIT_ASKPASS is called with a prompt: "Username for ..." or "Password for ..."

# Guard GIT_TOKEN explicitly: git runs this script in a sub-shell where the
# parent's set -u doesn't carry over, so an unset token would otherwise
# surface as a confusing HTTP 401 instead of a clear error.
if [ -z "${GIT_TOKEN:-}" ]; then
    echo "ERROR: GIT_TOKEN not set" >&2
    exit 1
fi

case "$1" in
    Username*) echo "x-access-token" ;;
    Password*) echo "${GIT_TOKEN}" ;;
esac
