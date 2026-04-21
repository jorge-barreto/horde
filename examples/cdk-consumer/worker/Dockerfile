# Optional project-specific image. `horde push` uses this to extend
# horde-worker-base:latest (built by horde from its embedded files —
# contains orc, claude-code, bd, and the entrypoint).
#
# Delete this file if you don't need project-specific tooling; `horde
# push` will then tag the base image directly.
FROM horde-worker-base:latest

# Example: install a project-specific CLI the workflows use.
# RUN apt-get update && apt-get install -y --no-install-recommends jq \
#  && rm -rf /var/lib/apt/lists/*
