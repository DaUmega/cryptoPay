#!/bin/bash
# Test harness for setup.sh, non-interactive mode only.
# It copies the repo to a temp dir (never touches the real .env),
# runs setup with no Docker start, and checks the generated .env.
# Full interactive + Docker flow is tested separately on Kali.
# Run: ./scripts/test-setup.sh
set -u
SRC=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
cp -r "$SRC"/* "$TMP"/ 2>/dev/null; rm -f "$TMP/.env" "$TMP/docker-compose.override.yml"
cd "$TMP" || exit 1
fail=0
run() { SETUP_PORT=18799 SETUP_START=0 ./scripts/setup.sh --non-interactive >/tmp/setup-test.log 2>&1; echo $?; }
rc=$(run); [ "$rc" = 0 ] || { echo "FAIL setup exit $rc"; cat /tmp/setup-test.log; exit 1; }
[ -f .env ] || { echo "FAIL no .env"; fail=1; }
perm=$(stat -c %a .env 2>/dev/null || stat -f %Lp .env)
[ "$perm" = 600 ] || { echo "FAIL .env mode $perm, want 600"; fail=1; }
token=$(sed -n 's/^API_TOKEN=//p' .env); secret=$(sed -n 's/^WEBHOOK_SECRET=//p' .env)
case "$token" in *[!0-9a-f]*|"") echo "FAIL API_TOKEN not hex: $token"; fail=1;; esac
[ "${#token}" = 64 ] || { echo "FAIL API_TOKEN length ${#token}, want 64"; fail=1; }
[ "${#secret}" = 64 ] || { echo "FAIL WEBHOOK_SECRET length ${#secret}, want 64"; fail=1; }
[ "$token" != "$secret" ] || { echo "FAIL token equals secret"; fail=1; }
grep -q '^PORT=18799$' .env || { echo "FAIL PORT not 18799"; fail=1; }
grep -q '"127.0.0.1:18799:18799"' docker-compose.yml || { echo "FAIL compose ports not 127.0.0.1:18799:18799"; fail=1; }
[ ! -f docker-compose.override.yml ] || { echo "FAIL override file must not be created (Compose merges ports and Docker fails)"; fail=1; }
grep -q '^PRICE_API_URL=$' .env || { echo "FAIL PRICE_API_URL not empty"; fail=1; }
# Second run must keep the same secrets (idempotency).
rc=$(run); [ "$rc" = 0 ] || { echo "FAIL second run exit $rc"; fail=1; }
[ "$(sed -n 's/^API_TOKEN=//p' .env)" = "$token" ] || { echo "FAIL second run changed API_TOKEN"; fail=1; }
# Invalid port must fail without writing a new .env.
rm -f .env
SETUP_PORT=99999 SETUP_START=0 ./scripts/setup.sh --non-interactive >/dev/null 2>&1; rc=$?
[ "$rc" != 0 ] && [ ! -f .env ] || { echo "FAIL invalid port accepted (rc=$rc)"; fail=1; }
# Help must work.
./scripts/setup.sh --help >/dev/null 2>&1 || { echo "FAIL --help"; fail=1; }
[ "$fail" = 0 ] && echo "PASS setup non-interactive" || echo "FAIL setup"
exit $fail
