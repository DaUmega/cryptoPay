#!/bin/bash
# Test harness for xmr-node-watch.sh. Uses only python3 + curl, and
# mock daemons / mock wallet RPC on localhost, never a real node.
# Run: ./scripts/test-xmr-node-watch.sh
set -u
cd "$(dirname "$0")/.."
RECORD=$(mktemp); STATE=$(mktemp); PIDS=""
trap 'kill $PIDS 2>/dev/null; rm -f "$RECORD" "$STATE"' EXIT
fail=0
mock() { # port height-or-FAIL (daemon) or wallet
  if [ "$2" = "wallet" ]; then
    MOCK_FAIL=${MOCK_FAIL:-0} MOCK_RECORD="$RECORD" python3 - "$1" <<'PY' &
import http.server, json, os, sys
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); body = self.rfile.read(n).decode()
        try: addr = json.loads(body)["params"]["address"]
        except Exception: addr = ""
        if os.environ.get("MOCK_FAIL") == "1":
            out = {"jsonrpc": "2.0", "id": "0", "error": {"message": "boom"}}
        else:
            open(os.environ["MOCK_RECORD"], "a").write(addr + "\n")
            out = {"jsonrpc": "2.0", "id": "0", "result": {}}
        data = json.dumps(out).encode(); self.send_response(200); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
  else
    python3 - "$1" "$2" <<'PY' &
import http.server, json, sys
port, height = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/get_height":
            data = json.dumps({"height": int(height), "status": "OK"}).encode()
            self.send_response(200); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
        else: self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
  fi
  PIDS="$PIDS $!"
}
run_watch() { # returns exit code; record file is $RECORD
  XMR_NODE_LIST="$1" XMR_WALLET_RPC_URL=http://127.0.0.1:18988/json_rpc \
    XMR_NODE_STATE="$STATE" ./scripts/xmr-node-watch.sh >/dev/null 2>&1; echo $?
}
# Scenario 1: current healthy, within lag -> no switch.
mock 18901 100; mock 18902 105; mock 18988 wallet; sleep 1
echo "http://127.0.0.1:18901" > "$STATE"; : > "$RECORD"
rc=$(run_watch "127.0.0.1:18901 127.0.0.1:18902")
[ "$rc" = 0 ] && [ ! -s "$RECORD" ] && echo "PASS healthy-no-switch" || { echo "FAIL healthy-no-switch rc=$rc record=$(cat "$RECORD")"; fail=1; }
kill $PIDS 2>/dev/null; PIDS=""; sleep 1
# Scenario 2: current lagging 20 blocks -> switch to best.
mock 18901 100; mock 18902 120; mock 18988 wallet; sleep 1
echo "http://127.0.0.1:18901" > "$STATE"; : > "$RECORD"
rc=$(run_watch "127.0.0.1:18901 127.0.0.1:18902")
[ "$rc" = 0 ] && [ "$(cat "$RECORD")" = "http://127.0.0.1:18902" ] && [ "$(cat "$STATE")" = "http://127.0.0.1:18902" ] \
  && echo "PASS lagging-switch" || { echo "FAIL lagging-switch rc=$rc record=$(cat "$RECORD") state=$(cat "$STATE")"; fail=1; }
kill $PIDS 2>/dev/null; PIDS=""; sleep 1
# Scenario 3: current daemon down -> switch to best reachable.
mock 18902 120; mock 18903 115; mock 18988 wallet; sleep 1
echo "http://127.0.0.1:18901" > "$STATE"; : > "$RECORD"
rc=$(run_watch "127.0.0.1:18901 127.0.0.1:18902 127.0.0.1:18903")
[ "$rc" = 0 ] && [ "$(cat "$RECORD")" = "http://127.0.0.1:18902" ] \
  && echo "PASS down-switch" || { echo "FAIL down-switch rc=$rc record=$(cat "$RECORD")"; fail=1; }
kill $PIDS 2>/dev/null; PIDS=""; sleep 1
# Scenario 4: all nodes down -> exit 1, no switch, state unchanged.
mock 18988 wallet; sleep 1
echo "http://127.0.0.1:18901" > "$STATE"; : > "$RECORD"
rc=$(run_watch "127.0.0.1:18901 127.0.0.1:18902")
[ "$rc" = 1 ] && [ ! -s "$RECORD" ] && [ "$(cat "$STATE")" = "http://127.0.0.1:18901" ] \
  && echo "PASS all-down-no-switch" || { echo "FAIL all-down rc=$rc record=$(cat "$RECORD")"; fail=1; }
kill $PIDS 2>/dev/null; PIDS=""; sleep 1
# Scenario 5: set_daemon fails -> exit 1, state not updated.
mock 18901 100; mock 18902 120; MOCK_FAIL=1 mock 18988 wallet; sleep 1
echo "http://127.0.0.1:18901" > "$STATE"; : > "$RECORD"
rc=$(run_watch "127.0.0.1:18901 127.0.0.1:18902")
[ "$rc" = 1 ] && [ "$(cat "$STATE")" = "http://127.0.0.1:18901" ] \
  && echo "PASS set-daemon-fail" || { echo "FAIL set-daemon-fail rc=$rc state=$(cat "$STATE")"; fail=1; }
kill $PIDS 2>/dev/null; PIDS=""; sleep 1
# Scenario 6: empty node list -> exit 2.
rc=$(run_watch ""); [ "$rc" = 2 ] && echo "PASS empty-list" || { echo "FAIL empty-list rc=$rc"; fail=1; }
exit $fail
