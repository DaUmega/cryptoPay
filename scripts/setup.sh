#!/bin/bash
# Cryptopay turnkey setup for non-technical operators.
# One command, from the repo root: ./scripts/setup.sh
# It generates secrets, asks a few plain questions, starts the
# service with Docker, and helps you add pool addresses.
# It never creates a full Monero wallet, never touches a seed, never
# commits .env, and never overwrites an existing .env without
# asking. It can guide a view-only Monero wallet setup via
# scripts/setup-monero.sh (Step 7, skippable). Run it again any
# time to check the service or top up pools.
# Non-interactive mode (for automation, e.g. a web repo calling it):
#   ./scripts/setup.sh --non-interactive
# with SETUP_PORT, SETUP_PRICE_API_URL, SETUP_PUBLIC_URL,
# SETUP_LOCAL_BIND (y/n), SETUP_START (0/1) in the environment.
# In that mode it only writes .env (and starts Docker if asked),
# asks nothing, and adds no pool addresses.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
NONINTERACTIVE=0; [ "${1:-}" = "--non-interactive" ] && NONINTERACTIVE=1
[ "${1:-}" = "--help" ] && { sed -n '2,12p' "$0"; exit 0; }
say() { echo; echo "$1"; }
pause() { [ "$NONINTERACTIVE" = 1 ] && return 0; printf '%s' "Press Enter to continue... "; read -r _ || { echo "Input ended unexpectedly. Aborting setup."; exit 1; }; }
ask() { # ask VAR PROMPT DEFAULT -> sets VAR (empty allowed)
  local var=$1 prompt=$2 def=${3:-} ans
  if [ "$NONINTERACTIVE" = 1 ]; then eval "$var=\$def"; return 0; fi
  printf '%s' "$prompt"; read -r ans || { echo "Input ended unexpectedly. Aborting setup."; exit 1; }; [ -z "$ans" ] && ans="$def"; eval "$var=\$ans"
}
ask_yn() { # ask_yn VAR PROMPT DEFAULT(y/n) -> sets VAR to y or n
  local var=$1 prompt=$2 def=${3:-n} ans
  if [ "$NONINTERACTIVE" = 1 ]; then eval "$var=\$def"; return 0; fi
  while true; do printf '%s' "$prompt"; read -r ans || { echo "Input ended unexpectedly. Aborting setup."; exit 1; }; [ -z "$ans" ] && ans="$def"
    case "$ans" in y|Y|yes) eval "$var=y"; return 0;; n|N|no) eval "$var=n"; return 0;; *) echo "Please answer y or n.";; esac
  done
}
secret() { openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom 2>/dev/null | od -An -tx1 | tr -d ' \n'; }
valid_url() { case "$1" in http://*|https://*) [ "${1#*://}" != "" ] && [ "${1// /}" = "$1" ];; *) return 1;; esac; }
get_env() { sed -n "s/^$1=//p" .env 2>/dev/null | head -n 1; }

say "Step 1: Checking what is installed."
command -v curl >/dev/null || { echo "Missing: curl. Install curl, then run this script again."; exit 1; }
# In non-interactive mode with SETUP_START=0, .env generation needs no
# Docker, so do not block on it (this also keeps the test harness
# runnable on machines without Docker).
if [ "$NONINTERACTIVE" = 1 ] && [ "${SETUP_START:-0}" != 1 ]; then
  echo "Found curl. Docker not needed in this mode (service not started)."
else
  command -v docker >/dev/null || { echo "Missing: docker. Install Docker (https://docs.docker.com/get-docker/), then run this script again."; exit 1; }
  docker compose version >/dev/null 2>&1 || { echo "Docker is installed but 'docker compose' is not. Install a recent Docker, then run this again."; exit 1; }
  echo "Found curl and Docker Compose."
fi
pause

say "Step 2: Secrets. These are generated for you, you do not need to invent them."
if [ -f .env ]; then
  ask_yn REGEN "A .env file already exists. Regenerate its secrets (old file is backed up)? [y/N] " n
  if [ "$REGEN" = y ]; then cp .env ".env.bak-$(date +%Y%m%d%H%M%S)"; echo "Old .env backed up."
    API_TOKEN=$(secret); WEBHOOK_SECRET=$(secret)
  else
    API_TOKEN=$(get_env API_TOKEN); WEBHOOK_SECRET=$(get_env WEBHOOK_SECRET)
    [ -n "$API_TOKEN" ] && [ -n "$WEBHOOK_SECRET" ] || { echo ".env exists but has no API_TOKEN or WEBHOOK_SECRET. Delete it or let this script regenerate it, then run again."; exit 1; }
    echo "Keeping the existing secrets."
  fi
else
  API_TOKEN=$(secret); WEBHOOK_SECRET=$(secret)
  echo "Generated a new API token and webhook secret."
fi
[ "${#API_TOKEN}" = 64 ] && [ "${#WEBHOOK_SECRET}" = 64 ] || { echo "Secret generation failed (need openssl or /dev/urandom)."; exit 1; }

say "Step 3: Port. Cryptopay listens here on this machine."
while true; do
  if [ "$NONINTERACTIVE" = 1 ]; then PORT=${SETUP_PORT:-18777}; else ask PORT "Port [18777]: " 18777; fi
  case "$PORT" in ''|*[!0-9]*) echo "Port must be a number."; [ "$NONINTERACTIVE" = 1 ] && exit 1; continue;; esac
  [ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || { echo "Port must be between 1024 and 65535."; [ "$NONINTERACTIVE" = 1 ] && exit 1; continue; }
  if command -v ss >/dev/null && ss -tln 2>/dev/null | grep -q ":$PORT "; then echo "Port $PORT is already in use on this machine. Pick another."; [ "$NONINTERACTIVE" = 1 ] && exit 1; continue; fi
  break
done

say "Step 4: Who talks to cryptopay?"
ask_yn LOCAL "Does your website run on this same machine? [Y/n] " y
if [ "$NONINTERACTIVE" = 1 ]; then [ "${SETUP_LOCAL_BIND:-y}" = "y" ] && LOCAL=y || LOCAL=n; fi
rm -f docker-compose.override.yml
if [ "$LOCAL" = y ]; then
  echo "Good: the port will only listen on localhost (127.0.0.1), so it is not reachable from the internet."
  BIND_PREFIX="127.0.0.1:"
else
  BIND_PREFIX=""
  echo "WARNING: the port will listen on all interfaces. Put a firewall or a reverse proxy with HTTPS in front of it, and never expose it to the open internet unprotected."
fi
# Write the port mapping into docker-compose.yml itself. A
# docker-compose.override.yml does NOT work here: Compose merges
# ports from both files, so an override would try to bind the same
# host port twice and Docker would fail with "address already in
# use". Editing the one ports line is the correct, single source.
[ -f docker-compose.yml.bak ] || cp docker-compose.yml docker-compose.yml.bak 2>/dev/null || true
sed -i -E "s|^([[:space:]]*- \").*(\")$|\1${BIND_PREFIX}${PORT}:${PORT}\2|" docker-compose.yml
grep -q "\"${BIND_PREFIX}${PORT}:${PORT}\"" docker-compose.yml || { echo "Could not update the ports line in docker-compose.yml. Fix it by hand, then run again."; exit 1; }
# Inside Docker, 127.0.0.1 is the cryptopay container itself, not the
# host, so a price API on the host needs host.docker.internal.
# Add the host-gateway mapping once (it survives later runs).
if ! grep -q "host.docker.internal" docker-compose.yml; then
  python3 -c "
import sys
p='docker-compose.yml'
s=open(p).read()
s=s.replace('    env_file: .env\n', '    env_file: .env\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n')
open(p,'w').write(s)
" || echo "Could not add extra_hosts to docker-compose.yml; if PRICE_API_URL points at the host, set it to host.docker.internal by hand."
fi
echo "docker-compose.yml now maps ${BIND_PREFIX}${PORT}:${PORT}."
pause

say "Step 5: Prices. Do you already run cryptoPriceAPI?"
echo "cryptoPriceAPI is a separate small service that gives cryptopay prices from several exchanges. If you do not have it, cryptopay uses CoinPaprika instead, and you can add cryptoPriceAPI later."
ask_yn HAS_PRICE "Do you already run cryptoPriceAPI? [y/N] " n
PRICE_API_URL=""
if [ "$NONINTERACTIVE" = 1 ]; then PRICE_API_URL=${SETUP_PRICE_API_URL:-}; HAS_PRICE=n; [ -n "$PRICE_API_URL" ] && HAS_PRICE=y; fi
if [ "$HAS_PRICE" = y ]; then
  while true; do
    if [ "$NONINTERACTIVE" = 1 ]; then break; fi
    ask PRICE_API_URL "Paste your cryptoPriceAPI URL (e.g. http://127.0.0.1:19080), or type skip: " ""
    [ "$PRICE_API_URL" = "skip" ] && { PRICE_API_URL=""; break; }
    valid_url "$PRICE_API_URL" || { echo "That does not look like an http(s) URL. Try again, or type skip."; continue; }
    if curl -s --max-time 10 "$PRICE_API_URL/health" | grep -q '"status"'; then echo "Reached cryptoPriceAPI. Good."; break; fi
    echo "Could not reach $PRICE_API_URL/health. Check it is running, then try again, or type skip to use CoinPaprika."
  done
else
  echo "Using the built-in CoinPaprika price fallback. To switch later, set PRICE_API_URL in .env and restart."
fi
# Docker rewrite: a localhost price URL works from the host shell
# (the health check above) but fails inside the container, where
# 127.0.0.1 is the container itself. Rewrite it to the host gateway.
case "$PRICE_API_URL" in
  http://127.0.0.1:*|http://localhost:*)
    PRICE_API_URL=$(echo "$PRICE_API_URL" | sed 's|://127.0.0.1:|://host.docker.internal:|; s|://localhost:|://host.docker.internal:|')
    echo "Rewrote PRICE_API_URL for Docker to $PRICE_API_URL (host.docker.internal is this machine, seen from inside the container)."
    ;;
esac
pause

say "Step 6: Pay page address (optional)."
echo "When a customer pays, cryptopay can send them to a pay page. If customers will open that page through a public address (for example https://pay.example.com), paste it now. Otherwise leave it blank and your website can use the relative pay link instead."
if [ "$NONINTERACTIVE" = 1 ]; then PUBLIC_URL=${SETUP_PUBLIC_URL:-}; else ask PUBLIC_URL "Public pay address, or blank: " ""; fi
if [ -n "$PUBLIC_URL" ]; then valid_url "$PUBLIC_URL" || { echo "Public address must start with http:// or https://. Leaving it blank."; PUBLIC_URL=""; }; fi
pause

say "Step 7: Monero (optional, skip this if you are not sure)."
echo "Monero is different from the other coins: cryptopay talks to a view-only wallet, it never holds your seed or spend key. You can set that wallet up here, guided, or skip Monero entirely and add it later with ./scripts/setup-monero.sh, without redoing this setup."
ask_yn WANT_XMR "Do you want to set up Monero now (guided view-only wallet + RPC service)? [y/N] " n
XMR_RPC_URL="http://monero-wallet-rpc:18088/json_rpc"; XMR_USER=""; XMR_PASS=""; XMR_NODES=""; HAS_XMR=n
if [ "$WANT_XMR" = y ] && [ "$NONINTERACTIVE" = 0 ]; then
  if ./scripts/setup-monero.sh --no-start; then
    HAS_XMR=y
    # setup-monero.sh wrote .monero-setup.env (no .env exists yet at
    # this step); Step 8 reads it, and Step 9 starts the RPC service.
    if [ -f .monero-setup.env ]; then
      XMR_RPC_URL=$(sed -n 's/^XMR_WALLET_RPC_URL=//p' .monero-setup.env | head -n 1)
      XMR_NODES=$(sed -n 's/^XMR_NODE_LIST=//p' .monero-setup.env | head -n 1)
      [ -n "$XMR_RPC_URL" ] || XMR_RPC_URL="http://monero-wallet-rpc:18088/json_rpc"
    fi
  else
    echo "Monero setup did not complete. Continuing without XMR; run ./scripts/setup-monero.sh later to add it."
  fi
elif [ "$WANT_XMR" = y ]; then
  echo "Non-interactive mode cannot guide a wallet setup (it needs a view key, a password, and node choices). Skipping Monero; run ./scripts/setup-monero.sh interactively later."
fi
pause

say "Step 8: Writing .env (permissions 600, never committed; .gitignore already lists .env)."
{
  echo "API_TOKEN=$API_TOKEN"; echo "WEBHOOK_SECRET=$WEBHOOK_SECRET"; echo "PORT=$PORT"; echo "BIND=0.0.0.0"; echo "DATA_DIR=/data"
  echo "PUBLIC_URL=$PUBLIC_URL"; echo "PRICE_API_URL=$PRICE_API_URL"
  echo "XMR_WALLET_RPC_URL=$XMR_RPC_URL"; echo "XMR_WALLET_RPC_USER=$XMR_USER"; echo "XMR_WALLET_RPC_PASS=$XMR_PASS"
  echo "INVOICE_EXPIRY_MIN=60"; echo "POLL_INTERVAL_SEC=10"
} > .env; chmod 600 .env
echo "Wrote .env."
# The Monero watchdog cron, if any, was already offered by
# setup-monero.sh, with the host perspective URL
# (http://127.0.0.1:18088), not the container perspective URL
# (http://monero-wallet-rpc:18088) written to .env above. Do not
# re-add it here with the container URL, or the host watchdog
# would never reach the RPC.
rm -f .monero-setup.env

if [ "$NONINTERACTIVE" = 1 ] && [ "${SETUP_START:-0}" != 1 ]; then echo "Non-interactive mode: .env written, service not started (SETUP_START=0)."; exit 0; fi

say "Step 9: Starting cryptopay with Docker."
docker compose up -d --build || { echo "Docker failed to start. Run 'docker compose logs' for the error, fix it, and run this script again."; exit 1; }
ok=0; for _ in $(seq 1 30); do if curl -s --max-time 5 "http://127.0.0.1:$PORT/healthz" | grep -q '"ok":true'; then ok=1; break; fi; sleep 2; done
[ "$ok" = 1 ] || { echo "Cryptopay did not answer healthz within a minute. Check: docker compose logs"; exit 1; }
echo "Cryptopay is up at http://127.0.0.1:$PORT/healthz"
if [ "$HAS_XMR" = y ]; then
  echo "Starting the Monero wallet RPC service (setup-monero.sh already generated the wallet)..."
  docker compose -f docker-compose.yml -f docker-compose.monero.yml up -d --build monero-wallet-rpc || echo "Monero RPC service failed to start. Cryptopay still works for other coins; check its logs and run ./scripts/setup-monero.sh again."
fi
pause

say "Step 10: Pool addresses. This is how cryptopay knows where to receive payments."
echo "For each coin you accept, you need receiving addresses, one per invoice, 20 to 50 per coin to start. An address is used once, then retired, so top them up when they run low."
echo "Notes for the tricky ones: XRP addresses need a 10 XRP reserve funded first; XLM addresses need a 1 XLM minimum balance; BCH addresses must start with bitcoincash:; ZEC addresses must be transparent (t1/t3), not shielded; the same address works across EVM coins (ETH, MATIC, ARB, OP, BASE, AVAX) but the pool is per coin, so add it to each coin you accept."
echo "Coins: BTC LTC ETH MATIC ARB OP BASE AVAX DOGE BCH DASH ZEC ADA XRP XLM TRX SOL USDC USDT XMR"
ask COINS "Which coins do you accept, separated by spaces (or blank to skip and add later): " ""
POOL_FAIL=0
for coin in $COINS; do
  case " BTC LTC ETH MATIC ARB OP BASE AVAX DOGE BCH DASH ZEC ADA XRP XLM TRX SOL USDC USDT XMR " in *" $coin "*) ;; *) echo "Skipping unknown coin: $coin"; continue;; esac
  [ "$coin" = "XMR" ] && [ "$HAS_XMR" != y ] && { echo "Skipping XMR: no wallet RPC was set up in step 7. Finish the README Monero setup first."; continue; }
  ask ADDRS "Paste $coin addresses, separated by spaces or commas: " ""
  [ -z "$ADDRS" ] && continue
  ADDRS=$(echo "$ADDRS" | tr ',' ' ')
  # Build a JSON array without jq: ["a","b"]
  arr=""; for a in $ADDRS; do arr="$arr\"$a\","; done; arr="[${arr%,}]"
  resp=$(curl -s --max-time 15 -X POST "http://127.0.0.1:$PORT/admin/pool" -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' -d "{\"coin\":\"$coin\",\"addresses\":$arr}")
  echo "$resp"
  case "$resp" in *'"added"'*) echo "Added for $coin (see 'added' above). If added is 0, an address was invalid, already in the pool, or in the wrong format; check it and try again.";; *) echo "Pool add failed for $coin. The response above is the reason."; POOL_FAIL=1;; esac
done
if [ "$POOL_FAIL" = 1 ]; then
  echo "SETUP NOT COMPLETE: at least one pool add failed, so those coins cannot receive payments yet. Fix the reason above (for example, check 'docker compose logs' for a /data permission error), then run this script again to add the missing addresses. Do not treat the service as ready until pool adds succeed."
  exit 1
fi
echo "Check pool counts any time with:"
echo "  curl http://127.0.0.1:$PORT/admin/pool -H \"Authorization: Bearer \$API_TOKEN\""
pause

say "Step 11: Webhooks, in plain language."
cat <<'EOF'
A webhook is how cryptopay tells your website "this invoice is paid",
so your website does not have to keep asking.
- Your website needs an endpoint that accepts POST JSON, for
  example https://your-site.com/webhooks/cryptopay.
- When your website creates an invoice, it passes that endpoint as
  "webhook_url" in the invoice request (see the example below).
- When the payment confirms, cryptopay POSTs JSON to that URL with
  invoice_id, merchant_ref, coin, address, amount, state
  ("confirmed" or "paid_late"), and txids.
- Security: cryptopay signs the raw request body with HMAC-SHA256
  using your WEBHOOK_SECRET, and sends it as the
  X-Cryptopay-Signature header. Your endpoint must recompute that
  signature over the raw body and compare it in constant time,
  and reject the request if it does not match. Only then mark the
  order paid. Return a 2xx status only after you have processed it;
  cryptopay retries at 0s, 5s, 30s, and 2 minutes if you do not.
- After payment, the pay page can send the customer back to your
  site if you also pass "return_url" when creating the invoice.
- You cannot test a confirmed webhook without a real payment, so
  make your first real payment a small one you control, and check
  your endpoint logs and the invoice state (GET /invoices/{id}).
EOF
echo
echo "Example invoice creation (run from this directory, after exporting your token):"
echo "  export API_TOKEN=\$(grep ^API_TOKEN= .env | cut -d= -f2)"
echo "  curl -X POST http://127.0.0.1:$PORT/invoices -H \"Authorization: Bearer \$API_TOKEN\" \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"coin\":\"BTC\",\"amount_usd\":10,\"merchant_ref\":\"order-123\",\"webhook_url\":\"https://your-site.com/webhooks/cryptopay\",\"return_url\":\"https://your-site.com/thank-you\"}'"
pause

say "Done. Production checklist:"
cat <<EOF
- Your API token and webhook secret are in .env (mode 600). Copy
  them into your website's own secret store or password manager.
  Do not share them, do not commit .env, and do not paste them
  into chat or screenshots.
- Back up your payment data regularly. With Docker it lives in the
  cryptopay-data volume; back it up with:
    docker run --rm -v cryptopay-data:/data -v "\$PWD:/backup" alpine tar czf /backup/cryptopay-data-backup.tar.gz -C /data .
  If you lose it, open invoices lose their address mapping (funds
  already sent are still on chain and recoverable from the
  addresses, but cryptopay will not know which invoice they were).
- Top up pool addresses when GET /admin/pool shows a coin running
  low, and sweep a Monero hot wallet to cold storage regularly.
- To update later: pull the latest code, then run
    docker compose up -d --build
- Health: http://127.0.0.1:$PORT/healthz
- Pool counts: curl http://127.0.0.1:$PORT/admin/pool -H "Authorization: Bearer \$API_TOKEN"
EOF
echo
echo "Your API token (copy it now, it is also in .env): $API_TOKEN"
echo "Your webhook secret (copy it now, it is also in .env): $WEBHOOK_SECRET"
