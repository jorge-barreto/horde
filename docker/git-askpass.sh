#!/bin/bash
# GIT_ASKPASS is called with a prompt: "Username for ..." or "Password for ..."
case "$1" in
    Username*) echo "x-access-token" ;;
    Password*) echo "${GIT_TOKEN}" ;;
esac
