#!/usr/bin/env sh
# AC-007 integration evidence — sanitized, manual, operator-invoked.
#
# This script is EXPLICITLY EXCLUDED FROM CI. It is run by hand, by the
# operator, against an installed OpenCode binary, to capture integration
# evidence that fixtures cannot provide (native authentication, live
# model execution). It never selects or invokes a provider/model
# automatically: the operator explicitly chooses and runs the
# provider/model step or skips it.
#
# Sanitization rules (enforced below):
#   - Generated transport credentials are printed ONLY as fingerprints.
#   - Provider API keys are never read, copied, or echoed.
#   - All captured output is redacted of credential-shaped values before
#     being written to the evidence directory.
#
# Usage:
#   scripts/ac007-integration-evidence.sh /path/to/opencode [evidence-dir]
set -eu

OPENCODE_BIN="${1:-}"
EVIDENCE_DIR="${2:-./ac007-evidence-$(date +%Y%m%d-%H%M%S)}"

if [ -z "$OPENCODE_BIN" ]; then
	echo "usage: $0 /path/to/opencode [evidence-dir]" >&2
	exit 2
fi
if [ ! -x "$OPENCODE_BIN" ]; then
	echo "not executable: $OPENCODE_BIN" >&2
	exit 2
fi

mkdir -p "$EVIDENCE_DIR"
EVIDENCE_DIR="$(cd "$EVIDENCE_DIR" && pwd)"

log() { printf '%s\n' "$*" | tee -a "$EVIDENCE_DIR/run.log"; }

# redact strips credential-shaped values (hex secrets, bearer tokens) from
# captured files before they are stored.
redact() {
	sed -E \
		-e 's/(OPENCODE_SERVER_(USERNAME|PASSWORD)=)[^ "'"'"']+/\1[REDACTED]/g' \
		-e 's/(Authorization: Bearer )[^ "'"'"']+/\1[REDACTED]/g' \
		"$1" >"$1.redacted" && mv "$1.redacted" "$1"
}

fingerprint() {
	# Print the secret's LENGTH only. No character of the value — not even
	# a prefix — is ever printed.
	printf 'len=%s' "${#1}"
}

log "AC-007 integration evidence run: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "binary: $OPENCODE_BIN (version output captured separately)"

# ── 1. Version + capability probe (provider-free) ────────────────────────
"$OPENCODE_BIN" --version >"$EVIDENCE_DIR/version.txt" 2>&1 || {
	log "FAIL: --version"
	exit 1
}
log "version: $(cat "$EVIDENCE_DIR/version.txt")"

PROBE_DIR="$(mktemp -d)"
(
	cd "$PROBE_DIR"
	set +e
	USER_UC="opencode-evidence-$$"
	PASS_UC="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	OPENCODE_SERVER_USERNAME="$USER_UC" OPENCODE_SERVER_PASSWORD="$PASS_UC" \
		"$OPENCODE_BIN" serve --hostname 127.0.0.1 --port 0 \
		>"serve.out" 2>"serve.err" &
	SERVE_PID=$!
	set -e
	log "probe serve pid: $SERVE_PID (credentials: $(fingerprint "$PASS_UC"))"

	# Wait for the printed listen address.
	for _ in $(seq 1 100); do
		if grep -q '127.0.0.1:' serve.out 2>/dev/null; then
			break
		fi
		sleep 0.1
	done
	EP="$(grep -o 'http://127\.0\.0\.1:[0-9]*' serve.out | head -1)"
	if [ -z "$EP" ]; then
		log "FAIL: probe server never printed its endpoint"
		kill "$SERVE_PID" 2>/dev/null || true
		exit 1
	fi
	log "endpoint: $EP"

	# Authenticated health + model inventory. Failures FAIL the script.
	if curl -sf -u "$USER_UC:$PASS_UC" "$EP/api/health" >health.json; then
		log "health: OK"
	else
		log "FAIL: authenticated health"
		kill "$SERVE_PID" 2>/dev/null || true
		exit 1
	fi
	if curl -sf -u "$USER_UC:$PASS_UC" "$EP/api/model" >models.json; then
		log "models: captured (inventory only; no provider call)"
	else
		log "FAIL: authenticated models"
		kill "$SERVE_PID" 2>/dev/null || true
		exit 1
	fi

	# Unauthenticated request must be rejected with exactly 401.
	UCODE="$(curl -s -o /dev/null -w '%{http_code}' "$EP/api/health")"
	log "unauthenticated health status: $UCODE (expected 401)"
	if [ "$UCODE" != "401" ]; then
		log "FAIL: unauthenticated health must return 401, got $UCODE"
		kill "$SERVE_PID" 2>/dev/null || true
		exit 1
	fi

	kill "$SERVE_PID" 2>/dev/null || true
	wait "$SERVE_PID" 2>/dev/null || true
)
redact "$PROBE_DIR/serve.out" 2>/dev/null || true
redact "$PROBE_DIR/serve.err" 2>/dev/null || true
mv "$PROBE_DIR"/*.json "$PROBE_DIR/serve.out" "$PROBE_DIR/serve.err" "$EVIDENCE_DIR/" 2>/dev/null || true
rm -rf "$PROBE_DIR"

# ── 2. Optional live model step (operator-invoked ONLY) ─────────────────
log ""
log "Live model execution is OPTIONAL and not run by this script."
log "To capture live-provider evidence, the operator explicitly runs and"
log "captures that step themselves (choosing provider and model), then"
log "stores sanitized output in: $EVIDENCE_DIR"
log "This script never selects or invokes a provider/model."

log ""
log "evidence directory: $EVIDENCE_DIR"
log "done."
