#!/bin/bash
# Add addresses from a file (one per line, # comments and blanks skipped)
# to a coin's pool. Run any time to top the pool up.
# Usage: PAYMENTS_URL=http://127.0.0.1:18777 API_TOKEN=... ./scripts/add-pool.sh BTC addresses.txt
set -e
if [ $# -ne 2 ]; then
  echo "usage: $0 COIN addresses.txt"
  exit 1
fi
COIN=$1
FILE=$2
URL=${PAYMENTS_URL:-http://127.0.0.1:18777}
: "${API_TOKEN:?set API_TOKEN}"
JSON=$(grep -v '^#' "$FILE" | grep -v '^[[:space:]]*$' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed 's/.*/"&"/' | paste -sd, -)
curl -s -X POST "$URL/admin/pool" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"coin\":\"$COIN\",\"addresses\":[$JSON]}"
echo
