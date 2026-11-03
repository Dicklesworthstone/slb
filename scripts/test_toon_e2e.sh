#!/bin/bash
# TOON output end-to-end checks for a built slb on PATH.
#
# TOON needs the toon_rust CLI (crate "tru"; the binary is "toon" since toon_rust
# v0.2.0, "tru" before). Without it the TOON checks are skipped and the run still
# passes. Set TOON_E2E_REQUIRE=1 to make a missing encoder, or a round trip that
# could not be checked, fail the run instead.
set -euo pipefail

TOON_E2E_REQUIRE=${TOON_E2E_REQUIRE:-0}

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }

# skip_or_fail logs a skipped check, or fails the run when TOON_E2E_REQUIRE=1.
skip_or_fail() {
    if [[ "$TOON_E2E_REQUIRE" == "1" ]]; then
        log "  FAIL: $* (TOON_E2E_REQUIRE=1)"
        exit 1
    fi
    log "  SKIP: $*"
}

# is_toon_rust uses the same identity check as slb: toon_rust's --help says
# "reference implementation in Rust"; a binary not named toon may instead print
# "tru X.Y.Z" or "toon_rust X.Y.Z" for --version. The Node.js toon CLI prints
# neither, so it is never used.
is_toon_rust() {
    local bin=$1 out base
    [[ -f "$bin" && -x "$bin" ]] || return 1
    out=$("$bin" --help 2>&1 || true)
    if grep -qi 'reference implementation in rust' <<<"$out"; then
        return 0
    fi
    base=$(basename "$bin" | tr '[:upper:]' '[:lower:]')
    if [[ "$base" == "toon" || "$base" == "toon.exe" ]]; then
        return 1
    fi
    out=$("$bin" --version 2>&1 | tr '[:upper:]' '[:lower:]' || true)
    [[ "$out" == "tru "* || "$out" == "toon_rust "* ]]
}

# find_toon_rust prints the encoder slb would use: TOON_TRU_BIN, TOON_BIN or
# TRU_PATH if set (and nothing if that binary is not toon_rust), else tru then
# toon on PATH, else tru then toon in the common locations.
find_toon_rust() {
    local var cand name dir
    for var in TOON_TRU_BIN TOON_BIN TRU_PATH; do
        cand=${!var:-}
        if [[ -n "$cand" ]]; then
            is_toon_rust "$cand" && { echo "$cand"; return 0; }
            return 1
        fi
    done
    for name in tru toon; do
        cand=$(command -v "$name" 2>/dev/null || true)
        if [[ -n "$cand" ]] && is_toon_rust "$cand"; then
            echo "$cand"
            return 0
        fi
    done
    for name in tru toon; do
        for dir in "${HOME:-}/.local/bin" "${HOME:-}/bin" /usr/local/bin \
            /data/projects/toon_rust/target/release /data/projects/toon_rust/target/debug; do
            if is_toon_rust "$dir/$name"; then
                echo "$dir/$name"
                return 0
            fi
        done
    done
    return 1
}

log "=========================================="
log "SLB TOON INTEGRATION E2E TEST"
log "=========================================="

# Phase 1: Prerequisites
log ""
log "Phase 1: Prerequisites"
command -v slb || { log "FAIL: slb not found"; exit 1; }
log "  PASS: slb found at $(command -v slb)"

# Check for the toon_rust CLI (TOON encoder)
HAS_TRU="false"
if TRU_CMD=$(find_toon_rust); then
    log "  PASS: toon_rust CLI found at $TRU_CMD"
    HAS_TRU="true"
else
    TRU_CMD=""
    if [[ "$TOON_E2E_REQUIRE" == "1" ]]; then
        log "  FAIL: toon_rust CLI (tru or toon) not found (TOON_E2E_REQUIRE=1)"
        exit 1
    fi
    log "  WARN: toon_rust CLI (tru or toon) not found; TOON checks will be skipped"
fi

# Phase 2: Basic functionality
log ""
log "Phase 2: Basic Functionality"

# Test --output flag
json_result=$(slb version --output json 2>/dev/null)
if [[ -n "$json_result" && "$json_result" == "{"* ]]; then
    log "  PASS: JSON output works"
else
    log "  FAIL: JSON output failed"
    exit 1
fi

# Test TOON output
toon_result=$(slb version --output toon 2>/dev/null || true)
if [[ -n "$toon_result" && "$toon_result" != "{"* ]]; then
    log "  PASS: TOON output works (not JSON)"
else
    if [[ "$HAS_TRU" == "true" ]]; then
        log "  FAIL: TOON output returned JSON or empty"
        exit 1
    fi
    log "  WARN: toon_rust CLI missing; TOON output fell back to JSON"
fi

# Test -t shorthand
toon_short=$(slb version -t 2>/dev/null || true)
if [[ "$toon_short" == "$toon_result" ]]; then
    log "  PASS: -t shorthand works"
else
    log "  WARN: -t shorthand gives different result"
fi

# Phase 3: Round-trip verification
log ""
log "Phase 3: Round-trip Verification"

json_output=$(slb version --output json 2>/dev/null)
toon_output=$(slb version --output toon 2>/dev/null || true)

# Decode TOON back to JSON using the toon_rust CLI found in Phase 1
if [[ -z "$TRU_CMD" ]]; then
    skip_or_fail "Cannot verify round-trip without the toon_rust CLI"
fi

if [[ -n "$TRU_CMD" && -n "$toon_output" ]]; then
    decoded=$(echo "$toon_output" | "$TRU_CMD" -d 2>/dev/null || echo "")
    if [[ -n "$decoded" ]]; then
        orig_sorted=$(echo "$json_output" | jq -S . 2>/dev/null || echo "$json_output")
        decoded_sorted=$(echo "$decoded" | jq -S . 2>/dev/null || echo "$decoded")

        if [[ "$orig_sorted" == "$decoded_sorted" ]]; then
            log "  PASS: Round-trip preserves data"
        else
            log "  FAIL: Round-trip mismatch"
            log "  Original: $orig_sorted"
            log "  Decoded:  $decoded_sorted"
            exit 1
        fi
    else
        if [[ "$TOON_E2E_REQUIRE" == "1" ]]; then
            log "  FAIL: Could not decode TOON output (TOON_E2E_REQUIRE=1)"
            exit 1
        fi
        log "  WARN: Could not decode TOON output"
    fi
fi

# Phase 4: Token savings
log ""
log "Phase 4: Token Savings Verification"

if [[ "$HAS_TRU" == "true" && -n "$json_output" && -n "$toon_output" && "$toon_output" != "{"* ]]; then
    json_chars=$(echo -n "$json_output" | wc -c)
    toon_chars=$(echo -n "$toon_output" | wc -c)
    savings=$(( 100 - (toon_chars * 100 / json_chars) ))
    log "  JSON: $json_chars chars, TOON: $toon_chars chars, Savings: ${savings}%"

    if [[ $savings -ge 20 ]]; then
        log "  PASS: Token savings >= 20%"
    else
        log "  WARN: Token savings < 20% (got ${savings}%)"
    fi
fi

# Phase 5: Help text verification
log ""
log "Phase 5: Help Text Verification"

help_output=$(slb --help 2>&1)
if echo "$help_output" | grep -q "toon"; then
    log "  PASS: --help mentions toon format"
else
    log "  FAIL: --help does not mention toon format"
    exit 1
fi

if echo "$help_output" | grep -q "\-t,"; then
    log "  PASS: --help mentions -t flag"
else
    log "  WARN: --help does not explicitly show -t flag"
fi

log ""
log "=========================================="
log "SLB TOON E2E TESTS COMPLETE"
log "=========================================="
