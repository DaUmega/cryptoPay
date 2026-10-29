#!/bin/bash
# Guided Monero view-only wallet + RPC service setup for cryptopay.
# One binary per container: cryptopay stays a single Go binary, and
# monero-wallet-rpc runs as its own service in the same compose stack
# (docker-compose.monero.yml), started only when Monero is enabled.
#
# This script never creates a full wallet and never touches a seed.
# If you have no full Monero wallet yet, create one first (Monerujo,
# GUI, or CLI), write down its seed yourself, then come back here.
# It never logs, prints, or stores a view key, a primary address, or
# a wallet password: the address and view key are typed only at the
# wallet CLI's own prompts, and the password is typed once, hidden,
# and written only to monero-wallets/viewonly.pass (mode 600).
#
# Usage, from the repo root:
#   ./scripts/setup-monero.sh                 # guided, starts the RPC service
#   ./scripts/setup-monero.sh --no-start      # guided, but do not start Docker yet
#                                             # (used by scripts/setup.sh, which
#                                             # starts everything in its own step)
#   ./scripts/setup-monero.sh --already-have-rpc
#                                             # skip wallet generation and the
#                                             # service; record an existing RPC
#                                             # URL + node list in .env instead
#                                             # (e.g. a host systemd service)
# Non-interactive mode is refused for wallet generation, because a
# human must supply the view key, choose a password, and choose
# nodes. --already-have-rpc is the only mode that can run
# non-interactively, with SETUP_MONERO_RPC_URL and
# SETUP_MONERO_NODES in the environment.
#
# After this script, cryptopay talks to the RPC as
# http://monero-wallet-rpc:18088/json_rpc (inside Docker), while
# host tools (the watchdog) use http://127.0.0.1:18088/json_rpc,
# because the service publishes its port only on localhost.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
MONERO_VERSION=0.18.5.4
START=1; ALREADY=0; NONINTERACTIVE=0
for a in "$@"; do case "$a" in
  --no-start) START=0;;
  --already-have-rpc) ALREADY=1;;
  --non-interactive) NONINTERACTIVE=1;;
  --help) sed -n '2,32p' "$0"; exit 0;;
  *) echo "Unknown option: $a (see --help)"; exit 1;;
esac; done
ask() { local var=$1 prompt=$2 def=${3:-} ans; printf '%s' "$prompt"; read -r ans || { echo "Input ended unexpectedly."; exit 1; }; [ -z "$ans" ] && ans="$def"; eval "$var=\$ans"; }
ask_yn() { local var=$1 prompt=$2 def=${3:-n} ans; while true; do printf '%s' "$prompt"; read -r ans || { echo "Input ended unexpectedly."; exit 1; }; [ -z "$ans" ] && ans="$def"; case "$ans" in y|Y|yes) eval "$var=y"; return 0;; n|N|no) eval "$var=n"; return 0;; *) echo "Please answer y or n.";; esac; done; }
# update_env KEY VALUE: replace or append one line in .env, never print the value.
update_env() { python3 - "$1" "$2" <<'PY' 2>/dev/null || return 1
import sys
k, v = sys.argv[1], sys.argv[2]
try: lines = open('.env').read().splitlines()
except FileNotFoundError: lines = []
out, seen = [], False
for line in lines:
    if line.startswith(k + '='): out.append(k + '=' + v); seen = True
    else: out.append(line)
if not seen: out.append(k + '=' + v)
open('.env', 'w').write('\n'.join(out) + '\n')
PY
}
# node_info HOSTPORT -> "height synchronized" or empty on any failure.
# Synchronized is required: a syncing node would stall the wallet.
node_info() { curl -s --max-time 10 -X POST "http://$1/json_rpc" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":"0","method":"get_info"}' 2>/dev/null | python3 -c '
import json, sys
try:
    r = json.load(sys.stdin)["result"]
    assert r["nettype"] == "mainnet" and r["status"] == "OK" and r.get("synchronized")
    print(r["height"])
except Exception:
    sys.exit(1)
' 2>/dev/null; }
# test_nodes LIST -> first good node, or empty. Prints nothing else.
test_nodes() { for n in $1; do h=$(node_info "$n") && [ -n "$h" ] && { echo "$n"; return 0; }; done; return 1; }

if [ "$ALREADY" = 1 ]; then
  if [ "$NONINTERACTIVE" = 1 ]; then RPC_URL=${SETUP_MONERO_RPC_URL:-}; NODES=${SETUP_MONERO_NODES:-}; else
    ask RPC_URL "Existing wallet RPC URL [http://127.0.0.1:18088/json_rpc]: " "http://127.0.0.1:18088/json_rpc"
    echo "Node list, 1 to 10 nodes, space separated (example: nodes.hashvault.pro:18081 node.monerodevs.org:18089). These are nodes you chose, not a list fetched at runtime."
    ask NODES "Node list: " ""
  fi
  [ -n "$NODES" ] || { echo "A node list is required for the watchdog, even with an existing RPC. Aborting."; exit 1; }
  n=$(echo "$NODES" | wc -w); [ "$n" -ge 1 ] && [ "$n" -le 10 ] || { echo "Node list must have 1 to 10 nodes, got $n."; exit 1; }
  [ -f .env ] || { echo "No .env yet. Run ./scripts/setup.sh first, then this script again."; exit 1; }
  update_env XMR_WALLET_RPC_URL "$RPC_URL"; update_env XMR_NODE_LIST "$NODES"; chmod 600 .env
  echo "Recorded existing RPC and node list in .env. Cryptopay will use it after a restart."
  exit 0
fi
[ "$NONINTERACTIVE" = 1 ] && { echo "Wallet generation needs a human (view key, password, node choice). Run this script interactively, or use --already-have-rpc with env vars."; exit 1; }
command -v curl >/dev/null || { echo "Missing: curl."; exit 1; }
command -v python3 >/dev/null || { echo "Missing: python3 (used only to parse node replies and edit .env)."; exit 1; }
# Docker is checked after the node list, before any wallet work, so
# a bad node list fails fast, and a missing Docker does not waste
# a wallet generation step.

echo
echo "Monero setup, guided. This script will:"
echo "  1. Ask for 1 to 10 nodes you chose, and test each one."
echo "  2. Guide a view-only wallet generation from your existing full wallet."
echo "     It never creates a full wallet and never touches a seed."
echo "  3. Start monero-wallet-rpc as its own compose service, and"
echo "     optionally add the node watchdog cron (every 5 minutes)."
echo "You can stop here with Ctrl-C and run this script again later;"
echo "nothing is started until the wallet file exists."
echo
# --- nodes first, so a bad list fails fast, before any wallet work ---
# Docker is checked after the node loop, before restore height.
while true; do
  echo "Node list, 1 to 10 nodes, space separated, host:port each."
  echo "Example: nodes.hashvault.pro:18081 node.monerodevs.org:18089"
  echo "Pick nodes yourself (monero.fail lists public ones). The script"
  echo "tests each one and keeps only nodes that answer, are mainnet,"
  echo "and are synchronized. A node can hide a payment but cannot"
  echo "forge a confirmed one, because the wallet verifies itself."
  ask NODES "Node list: " ""
  n=$(echo "$NODES" | wc -w); [ "$n" -ge 1 ] && [ "$n" -le 10 ] || { echo "Need 1 to 10 nodes, got $n. Try again."; continue; }
  good=""; best=0; first=""
  for node in $NODES; do h=$(node_info "$node") || { echo "  $node: down, unsynced, or not mainnet, skipping."; continue; }
    echo "  $node: ok, height $h"; good="$good $node"; [ -z "$first" ] && first="$node"; [ "$h" -gt "$best" ] && best="$h"
  done
  good=$(echo "$good" | sed 's/^ //')
  [ -n "$good" ] || { echo "No node in the list answered usefully. Try a different list."; continue; }
  NODES="$good"; DAEMON="$first"; BEST="$best"; break
done
# --- Docker check, after nodes, before any wallet work ---
command -v docker >/dev/null || { echo "Missing: docker. Install Docker, then run again."; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker is installed but 'docker compose' is not."; exit 1; }
# --- restore height: default is best minus 1000, user can override ---
while true; do
  def=$((BEST - 1000)); [ "$def" -lt 0 ] && def=0
  ask HEIGHT "Restore height [$def, or your wallet creation height]: " "$def"
  case "$HEIGHT" in ''|*[!0-9]*) echo "Height must be a number."; continue;; esac
  [ "$HEIGHT" -le "$BEST" ] || { echo "Height is above the current height $BEST. Use a lower one."; continue; }
  break
done
# --- wallet CLI for the host arch, verified, reused if already present ---
mkdir -p monero-wallets; chmod 700 monero-wallets
CLI=monero-cli/monero-wallet-cli
if [ -x "$CLI" ] && "$CLI" --version 2>/dev/null | grep -q "$MONERO_VERSION"; then echo "Reusing existing Monero CLI ($MONERO_VERSION)."; else
  case "$(uname -m)" in
    x86_64) SUFFIX=x64; HASH=ba2928962c101b609434bf8c1b43ad59ebe96c5525977284ca599d8bf0f02d10;;
    aarch64) SUFFIX=armv8; HASH=d20d71e6522b41eb8e549c3425f7f4622c9c9a448fc7147525393fd0a6b59703;;
    armv7l) SUFFIX=armv7; HASH=124883ebd8b3e19fb9000bc051dd9a28958a1a383455af78b539362b0d59094f;;
    *) echo "Unsupported arch for Monero: $(uname -m)"; exit 1;;
  esac
  FILE="monero-linux-${SUFFIX}-v${MONERO_VERSION}.tar.bz2"; TMP=$(mktemp -d)
  echo "Downloading Monero CLI $MONERO_VERSION ($SUFFIX), verifying its hash..."
  curl -fsSL -o "$TMP/$FILE" "https://downloads.getmonero.org/cli/$FILE" || { echo "Download failed."; rm -rf "$TMP"; exit 1; }
  echo "$HASH  $TMP/$FILE" | sha256sum -c - || { echo "Hash mismatch, refusing to use this download."; rm -rf "$TMP"; exit 1; }
  mkdir -p monero-cli; tar -xjf "$TMP/$FILE" -C "$TMP"; cp "$TMP"/monero-*/monero-wallet-cli monero-cli/; cp "$TMP"/monero-*/monero-wallet-rpc monero-cli/ 2>/dev/null || true; rm -rf "$TMP"
  chmod +x monero-cli/monero-wallet-cli; echo "Monero CLI verified and saved in monero-cli/."
fi
# --- view-only wallet generation, or reuse an existing one ---
if [ -f monero-wallets/viewonly ] && [ -f monero-wallets/viewonly.keys ]; then
  echo "A view-only wallet already exists in monero-wallets/viewonly. Reusing it, not generating a new one."
  [ -f monero-wallets/viewonly.pass ] || { echo "It has no password file. Create one now, with the wallet password, or the RPC cannot open it."; }
else
  echo
  echo "Now the wallet CLI will ask you, interactively, for:"
  echo "  - your full wallet's primary address"
  echo "  - its view key"
  echo "  - a wallet password (you will type it at the prompt)"
  echo "Type those values only at those prompts. They are not logged,"
  echo "not written to .env, and not passed on any command line."
  echo "The view-only wallet cannot spend, even if this machine is"
  echo "compromised, because it has no spend key. Your full wallet"
  echo "stays where it is, and you sweep from it yourself."
  printf '%s' "Wallet password, hidden (this is also the RPC password file): "; read -rs PASS1 || exit 1; echo
  printf '%s' "Same password again: "; read -rs PASS2 || exit 1; echo
  [ "$PASS1" = "$PASS2" ] && [ -n "$PASS1" ] || { echo "Passwords did not match or were empty. Run again."; exit 1; }
  printf '%s' "$PASS1" > monero-wallets/viewonly.pass; chmod 600 monero-wallets/viewonly.pass; unset PASS1 PASS2
  echo "Password file written (mode 600). Now generating the view-only wallet..."
  "$CLI" --generate-from-view-key monero-wallets/viewonly --password-file monero-wallets/viewonly.pass --restore-height "$HEIGHT" --daemon-address "$DAEMON" || { echo "Wallet generation failed. Check the address, view key, and height, delete any partial monero-wallets/viewonly* files, and run again."; exit 1; }
  chmod 600 monero-wallets/viewonly monero-wallets/viewonly.keys monero-wallets/viewonly.pass
  echo "View-only wallet generated."
fi
# --- record config, start the service, check it answers ---
[ -f .env ] || { echo "No .env yet. Writing the Monero values to .monero-setup.env instead; scripts/setup.sh will copy them into .env in its Step 8."; { echo "XMR_WALLET_RPC_URL=http://monero-wallet-rpc:18088/json_rpc"; echo "XMR_NODE_LIST=$NODES"; echo "XMR_DAEMON=$DAEMON"; } > .monero-setup.env; chmod 600 .monero-setup.env; }
if [ -f .env ]; then update_env XMR_WALLET_RPC_URL "http://monero-wallet-rpc:18088/json_rpc"; update_env XMR_NODE_LIST "$NODES"; update_env XMR_DAEMON "$DAEMON"; chmod 600 .env; fi
if [ "$START" = 1 ]; then
  echo "Starting monero-wallet-rpc (compose service, port only on localhost)..."
  docker compose -f docker-compose.yml -f docker-compose.monero.yml up -d --build monero-wallet-rpc || { echo "Docker failed to start the RPC service. Check: docker compose -f docker-compose.yml -f docker-compose.monero.yml logs monero-wallet-rpc"; exit 1; }
  ok=0; for _ in $(seq 1 60); do
    if curl -s --max-time 5 -X POST http://127.0.0.1:18088/json_rpc -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":"0","method":"get_height"}' | grep -q '"result"'; then ok=1; break; fi
    sleep 2
  done
  [ "$ok" = 1 ] || { echo "Wallet RPC did not answer within 2 minutes. Check its logs, and that the password file matches the wallet password."; echo "  docker compose -f docker-compose.yml -f docker-compose.monero.yml logs monero-wallet-rpc"; exit 1; }
  echo "Wallet RPC is answering on http://127.0.0.1:18088 (host) and http://monero-wallet-rpc:18088 (inside Docker)."
  echo "It may still be refreshing from height $HEIGHT to $BEST; XMR payments will appear once it catches up."
else
  echo "Skipping Docker start (--no-start). Run this script again without --no-start, or start the service with:"
  echo "  docker compose -f docker-compose.yml -f docker-compose.monero.yml up -d --build monero-wallet-rpc"
fi
# --- watchdog cron, host perspective (localhost port, not the service name) ---
if [ "$NONINTERACTIVE" = 0 ]; then
  CRON="*/5 * * * * XMR_NODE_LIST=\"$NODES\" XMR_WALLET_RPC_URL=\"http://127.0.0.1:18088/json_rpc\" $ROOT/scripts/xmr-node-watch.sh >> $ROOT/xmr-node-watch.log 2>&1"
  echo "Watchdog cron line (host perspective, localhost port):"; echo "  $CRON"
  ask_yn ADD_CRON "Add it to your crontab now? [y/N] " n
  if [ "$ADD_CRON" = y ]; then (crontab -l 2>/dev/null | grep -v "xmr-node-watch.sh"; echo "$CRON") | crontab - && echo "Cron installed. Check with: crontab -l"; fi
fi
echo
echo "Monero setup done. Next: add XMR subaddresses to the pool."
echo "If cryptopay is running, generate them with the wallet RPC and add"
echo "them via the pool step in setup.sh, or run this script again later"
echo "after cryptopay is up. The view-only wallet tracks every"
echo "subaddress of its account automatically, so no other wallet"
echo "step is needed for new subaddresses."
