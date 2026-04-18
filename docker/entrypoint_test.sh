#!/bin/bash
# Unit tests for docker/entrypoint.sh. No external deps — pure bash.
# Run directly: bash docker/entrypoint_test.sh
# Exits non-zero on failure so CI/Makefile can gate on it.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"

FAIL=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; FAIL=1; }

# Each test runs entrypoint.sh under a fake PATH so git/orc/aws are stubs
# that record their invocations to a log file. The stubs exit 0 so we can
# observe what the entrypoint *would* have done without hitting the network.

setup_stubs() {
    local dir="$1"
    mkdir -p "$dir/bin"

    cat > "$dir/bin/git" <<'EOF'
#!/bin/bash
echo "git $*" >> "$STUB_LOG"
case "$1" in
    symbolic-ref) echo "refs/remotes/origin/main" ;;
esac
exit 0
EOF
    cat > "$dir/bin/orc" <<'EOF'
#!/bin/bash
echo "orc $*" >> "$STUB_LOG"
exit "${STUB_ORC_EXIT:-0}"
EOF
    cat > "$dir/bin/aws" <<'EOF'
#!/bin/bash
echo "aws $*" >> "$STUB_LOG"
exit 0
EOF
    chmod +x "$dir/bin/git" "$dir/bin/orc" "$dir/bin/aws"
}

# run_entrypoint <fast_path>: if fast_path=1 the tests pre-create
# /workspace/.git so the script skips clone (exercises ORC_CMD
# assembly + ECS branch). If fast_path=0 the clone branch runs so
# BRANCH checkout logic is observable. Returns the tmpdir on stdout.
run_entrypoint() {
    local fast_path="${1:-1}"
    local tmp; tmp=$(mktemp -d)
    setup_stubs "$tmp"
    export STUB_LOG="$tmp/log"
    : > "$STUB_LOG"

    mkdir -p "$tmp/workspace"
    if [ "$fast_path" = "1" ]; then
        mkdir -p "$tmp/workspace/.git"
    fi

    local patched="$tmp/entrypoint-patched.sh"
    sed "s|cd /workspace|cd $tmp/workspace|g; s|/workspace/\\.git|$tmp/workspace/.git|g; s|git config --global --add safe.directory /workspace|git config --global --add safe.directory $tmp/workspace|g" "$ENTRYPOINT" > "$patched"
    chmod +x "$patched"

    PATH="$tmp/bin:$PATH" bash "$patched" >"$tmp/out" 2>"$tmp/err"
    local rc=$?
    echo "$tmp"
    return $rc
}

# -- Test 1: missing REPO_URL -> exit 3 with clear message
t1_missing_repo_url() {
    local out
    out=$(REPO_URL= TICKET=T GIT_TOKEN=tok bash "$ENTRYPOINT" 2>&1)
    local rc=$?
    if [ "$rc" -eq 3 ] && [[ "$out" == *"REPO_URL not set"* ]]; then
        pass "missing REPO_URL exits 3 with clear error"
    else
        fail "missing REPO_URL: rc=$rc out=$out"
    fi
}

# -- Test 2: missing TICKET -> exit 3
t2_missing_ticket() {
    local out
    out=$(REPO_URL=r TICKET= GIT_TOKEN=tok bash "$ENTRYPOINT" 2>&1)
    local rc=$?
    if [ "$rc" -eq 3 ] && [[ "$out" == *"TICKET not set"* ]]; then
        pass "missing TICKET exits 3"
    else
        fail "missing TICKET: rc=$rc out=$out"
    fi
}

# -- Test 3: missing GIT_TOKEN -> exit 3
t3_missing_git_token() {
    local out
    out=$(REPO_URL=r TICKET=T GIT_TOKEN= bash "$ENTRYPOINT" 2>&1)
    local rc=$?
    if [ "$rc" -eq 3 ] && [[ "$out" == *"GIT_TOKEN not set"* ]]; then
        pass "missing GIT_TOKEN exits 3"
    else
        fail "missing GIT_TOKEN: rc=$rc out=$out"
    fi
}

# -- Test 4: BRANCH unset -> default branch checkout path
t4_branch_unset_default_checkout() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok \
        run_entrypoint 0)
    if grep -q "git checkout main" "$tmp/log"; then
        pass "BRANCH unset falls back to default branch"
    else
        fail "BRANCH unset: did not checkout default; log=$(cat "$tmp/log")"
    fi
}

# -- Test 5: BRANCH set -> explicit checkout
t5_branch_set_explicit_checkout() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok BRANCH=feat/x \
        run_entrypoint 0)
    if grep -q "git checkout feat/x" "$tmp/log"; then
        pass "BRANCH set checks out the named branch"
    else
        fail "BRANCH=feat/x: wrong checkout; log=$(cat "$tmp/log")"
    fi
}

# -- Test 6: WORKFLOW unset -> orc command lacks -w flag
t6_workflow_unset_no_flag() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok \
        run_entrypoint)
    if grep -q "^orc run T-1 " "$tmp/log" && ! grep -q " -w " "$tmp/log"; then
        pass "WORKFLOW unset omits -w flag"
    else
        fail "WORKFLOW unset: unexpected orc args; log=$(cat "$tmp/log")"
    fi
}

# -- Test 7: WORKFLOW set -> orc command includes -w WORKFLOW
t7_workflow_set_passes_flag() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok WORKFLOW=plan \
        run_entrypoint)
    if grep -q "^orc run -w plan T-1 " "$tmp/log"; then
        pass "WORKFLOW=plan passes -w plan"
    else
        fail "WORKFLOW=plan: wrong orc args; log=$(cat "$tmp/log")"
    fi
}

# -- Test 8: ARTIFACTS_BUCKET unset -> no aws calls (docker path)
t8_no_bucket_no_aws() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok \
        run_entrypoint)
    if ! grep -q "^aws " "$tmp/log"; then
        pass "ARTIFACTS_BUCKET unset skips aws s3 block"
    else
        fail "unexpected aws calls without bucket; log=$(cat "$tmp/log")"
    fi
}

# -- Test 9: ARTIFACTS_BUCKET set -> aws s3 calls happen (ECS path)
t9_bucket_triggers_uploads() {
    local tmp
    tmp=$(REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok \
        ARTIFACTS_BUCKET=my-bucket RUN_ID=run123 \
        run_entrypoint)
    if grep -q "^aws s3 sync.*s3://my-bucket/horde-runs/run123/sessions" "$tmp/log"; then
        pass "ARTIFACTS_BUCKET triggers aws s3 session sync"
    else
        fail "ARTIFACTS_BUCKET: no session sync; log=$(cat "$tmp/log")"
    fi
}

# -- Test 10: orc exit code is propagated (ECS path)
t10_exit_code_propagated() {
    local tmp
    tmp=$(mktemp -d)
    setup_stubs "$tmp"
    export STUB_LOG="$tmp/log"
    export STUB_ORC_EXIT=42
    : > "$STUB_LOG"
    mkdir -p "$tmp/workspace/.git"
    local patched="$tmp/entrypoint-patched.sh"
    sed "s|cd /workspace|cd $tmp/workspace|g; s|/workspace/\\.git|$tmp/workspace/.git|g; s|git config --global --add safe.directory /workspace|git config --global --add safe.directory $tmp/workspace|g" "$ENTRYPOINT" > "$patched"
    PATH="$tmp/bin:$PATH" REPO_URL=example.com/r.git TICKET=T-1 GIT_TOKEN=tok \
        ARTIFACTS_BUCKET=b RUN_ID=rid \
        bash "$patched" >"$tmp/out" 2>"$tmp/err"
    local rc=$?
    unset STUB_ORC_EXIT
    if [ "$rc" -eq 42 ]; then
        pass "orc exit code propagates through ECS branch"
    else
        fail "orc exit propagation: rc=$rc"
    fi
}

t1_missing_repo_url
t2_missing_ticket
t3_missing_git_token
t4_branch_unset_default_checkout
t5_branch_set_explicit_checkout
t6_workflow_unset_no_flag
t7_workflow_set_passes_flag
t8_no_bucket_no_aws
t9_bucket_triggers_uploads
t10_exit_code_propagated

exit "$FAIL"
