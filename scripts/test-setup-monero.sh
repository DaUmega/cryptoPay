#!/bin/bash
# Tests for scripts/setup-monero.sh, without keys, wallets, or Docker.
# - bash syntax for setup-monero.sh and setup.sh
# - node_info: good mainnet synced node passes, unsynced / testnet /
#   down nodes fail, first good node is picked
# - --already-have-rpc --non-interactive writes .env correctly
# - interactive wallet generation refuses non-interactive mode
# Mock daemons are local python servers; no real Monero node is used.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
FAIL=0
ok() { echo "OK   $1"; }
fail() { echo "FAIL $1"; FAIL=1; }
bash -n scripts/setup-monero.sh && bash -n scripts/setup.sh && ok "syntax" || fail "syntax"
# Extract node_info + test_nodes from setup-monero.sh (functions only).
sed -n '/^# node_info/,/^test_nodes/p' scripts/setup-monero.sh > /tmp/test-xmr-funcs.sh
# test_nodes line itself is a function definition, keep it; drop the trailing comment line if present.
sed -i '/^# test_nodes/d' /tmp/test-xmr-funcs.sh 2>/dev/null || true
# Actually keep test_nodes: re-add it from the file.
grep -A1 '^test_nodes()' scripts/setup-monero.sh >> /tmp/test-xmr-funcs.sh
# Mock daemons: good on 18991, unsynced on 18992, testnet on 18993.
python3 - <<'PY' &
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
import threading
def serve(port, result):
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get('Content-Length', 0)); self.rfile.read(n)
            body = json.dumps({"jsonrpc":"2.0","id":"0","result":result}).encode()
            self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(body))); self.end_headers(); self.wfile.write(body)
        def log_message(self, *a): pass
    HTTPServer(('127.0.0.1', port), H).serve_forever()
for port, res in [
    (18991, {"nettype":"mainnet","status":"OK","synchronized":True,"height":3794000}),
    (18992, {"nettype":"mainnet","status":"OK","synchronized":False,"height":3794000}),
    (18993, {"nettype":"testnet","status":"OK","synchronized":True,"height":3794000}),
]:
    threading.Thread(target=serve, args=(port, res), daemon=True).start()
import time; time.sleep(10)
PY
MOCK_PID=$!
sleep 1
# shellcheck disable=SC1091
. /tmp/test-xmr-funcs.sh
[ "$(node_info 127.0.0.1:18991)" = "3794000" ] && ok "node_info good" || fail "node_info good"
[ -z "$(node_info 127.0.0.1:18992 2>/dev/null)" ] && ok "node_info unsynced rejected" || fail "node_info unsynced rejected"
[ -z "$(node_info 127.0.0.1:18993 2>/dev/null)" ] && ok "node_info testnet rejected" || fail "node_info testnet rejected"
[ -z "$(node_info 127.0.0.1:18994 2>/dev/null)" ] && ok "node_info down rejected" || fail "node_info down rejected"
[ "$(test_nodes "127.0.0.1:18992 127.0.0.1:18991")" = "127.0.0.1:18991" ] && ok "test_nodes picks first good" || fail "test_nodes picks first good"
kill $MOCK_PID 2>/dev/null; wait $MOCK_PID 2>/dev/null || true
# already-have-rpc, non-interactive, in a temp copy so .env is not touched.
TMP=$(mktemp -d); mkdir -p "$TMP/scripts"; cp scripts/setup-monero.sh "$TMP/scripts/"; printf 'API_TOKEN=x\n' > "$TMP/.env"; chmod 600 "$TMP/.env"
(cd "$TMP" && SETUP_MONERO_RPC_URL=http://127.0.0.1:18088/json_rpc SETUP_MONERO_NODES="a:18081 b:18089" bash scripts/setup-monero.sh --already-have-rpc --non-interactive >/dev/null 2>&1) && grep -q "XMR_WALLET_RPC_URL=http://127.0.0.1:18088/json_rpc" "$TMP/.env" && grep -q "XMR_NODE_LIST=a:18081 b:18089" "$TMP/.env" && ok "already-have-rpc writes .env" || fail "already-have-rpc writes .env"
# 11 nodes must be rejected.
(cd "$TMP" && SETUP_MONERO_RPC_URL=http://127.0.0.1:18088/json_rpc SETUP_MONERO_NODES="a b c d e f g h i j k" bash scripts/setup-monero.sh --already-have-rpc --non-interactive >/dev/null 2>&1) && fail "11 nodes rejected" || ok "11 nodes rejected"
rm -rf "$TMP" /tmp/test-xmr-funcs.sh
# interactive generation must refuse non-interactive mode.
bash scripts/setup-monero.sh --non-interactive >/dev/null 2>&1 && fail "non-interactive generation refused" || ok "non-interactive generation refused"
[ "$FAIL" = 0 ] && echo "TEST-SETUP-MONERO ALL GREEN" || { echo "TEST-SETUP-MONERO FAILURES"; exit 1; }
