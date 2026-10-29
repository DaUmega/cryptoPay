#!/bin/bash
# XMR daemon watchdog: one run, suitable for cron every 5 minutes.
# It does NOT touch cryptopay, keys, or the wallet file. It only asks
# the already-running view-only monero-wallet-rpc to switch daemons
# (set_daemon, trusted=false) when the current daemon is down or
# lagging. A remote node can hide payments but cannot forge a
# confirmed one (the wallet verifies cryptographically), so the
# failure mode this guards is a stalled wallet, not a forged payment.
#
# Config (env, same names cryptopay uses where they overlap):
#   XMR_NODE_LIST        space-separated daemons, operator-curated,
#                        e.g. "node1.example.com:18081 node2:18089".
#                        Bare host:port gets http://; https:// is kept.
#                        NOT fetched from a third party at runtime.
#   XMR_WALLET_RPC_URL   default http://127.0.0.1:18088/json_rpc
#   XMR_WALLET_RPC_USER, XMR_WALLET_RPC_PASS (wallet-RPC login only,
#                        never the wallet password)
#   XMR_NODE_STATE       file remembering the last chosen daemon,
#                        default ./xmr-node.state
#   XMR_NODE_MAX_LAG     blocks behind the best node before switching
#                        (default 10; Monero blocks are ~2 minutes)
#   XMR_NODE_TIMEOUT     seconds per daemon probe (default 10)
#   XMR_DAEMON_PROXY     optional, e.g. 127.0.0.1:9050 for .onion nodes
# Cron example:
#   */5 * * * * XMR_NODE_LIST="a:18081 b:18089" /path/to/cryptopay/scripts/xmr-node-watch.sh >> /var/log/xmr-node-watch.log 2>&1
# Limitation: the watchdog tracks the daemon it last chose in the
# state file; it does not query the wallet for its current daemon
# (wallet RPC has no such getter). If you switch daemons by hand,
# delete the state file so the next run re-anchors to the best node.
set -u
RPC=${XMR_WALLET_RPC_URL:-http://127.0.0.1:18088/json_rpc}
STATE=${XMR_NODE_STATE:-./xmr-node.state}
LAG=${XMR_NODE_MAX_LAG:-10}
TIMEOUT=${XMR_NODE_TIMEOUT:-10}
[ -n "${XMR_NODE_LIST:-}" ] || { echo "xmr-node-watch: XMR_NODE_LIST is empty, refusing to guess a daemon"; exit 2; }

norm() { case "$1" in http://*|https://*) echo "$1";; *) echo "http://$1";; esac; }
height() { # daemon base URL -> height, or empty on any failure
  curl -s --max-time "$TIMEOUT" "$1/get_height" 2>/dev/null |
    sed -n 's/.*"height"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n 1
}
set_daemon() { # full daemon URL -> 0 on success
  proxy=""; [ -n "${XMR_DAEMON_PROXY:-}" ] && proxy=",\"proxy\":\"$XMR_DAEMON_PROXY\""
  body="{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"set_daemon\",\"params\":{\"address\":\"$1\",\"trusted\":false$proxy}}"
  auth=(); [ -n "${XMR_WALLET_RPC_USER:-}" ] && auth=(-u "${XMR_WALLET_RPC_USER}:${XMR_WALLET_RPC_PASS:-}")
  resp=$(curl -s --max-time "$TIMEOUT" "${auth[@]}" -H 'Content-Type: application/json' -d "$body" "$RPC" 2>/dev/null) || return 1
  case "$resp" in *\"error\"*) return 1;; *\"result\"*) return 0;; *) return 1;; esac
}

best=""; best_h=0
for node in $XMR_NODE_LIST; do
  url=$(norm "$node"); h=$(height "$url")
  [ -n "$h" ] && [ "$h" -gt "$best_h" ] 2>/dev/null && { best="$url"; best_h=$h; }
done
[ -n "$best" ] || { echo "xmr-node-watch: no node in list answered, leaving daemon alone"; exit 1; }

cur=""; [ -f "$STATE" ] && cur=$(cat "$STATE" 2>/dev/null)
if [ -n "$cur" ]; then
  cur_h=$(height "$cur")
  if [ -n "$cur_h" ] && [ $((best_h - cur_h)) -le "$LAG" ] 2>/dev/null; then
    echo "xmr-node-watch: ok, current $cur at $cur_h, best $best_h"
    exit 0
  fi
  echo "xmr-node-watch: current $cur at ${cur_h:-down}, best $best at $best_h, switching"
else
  echo "xmr-node-watch: no state file, anchoring to best $best at $best_h"
fi
set_daemon "$best" || { echo "xmr-node-watch: set_daemon to $best failed, state not updated"; exit 1; }
echo "$best" > "$STATE"
echo "xmr-node-watch: switched to $best at $best_h"
