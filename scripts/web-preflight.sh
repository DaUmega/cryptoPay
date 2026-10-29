#!/bin/bash
# Preflight for a site that depends on cryptopay: fail fast if the
# payments service is not up, before the rest of the setup runs.
# Usage: PAYMENTS_URL=http://127.0.0.1:18777 ./scripts/web-preflight.sh
# A site's setup script (deploy.sh) calls this at the top, before building.
set -e
URL=${PAYMENTS_URL:-http://127.0.0.1:18777}
if ! curl -s --max-time 10 "$URL/healthz" | grep -q '"ok":true'; then
  echo "cryptopay is not reachable at $URL"
  echo "Start it first: cd cryptopay && docker compose up -d"
  echo "Then re-run this setup."
  exit 1
fi
echo "cryptopay OK at $URL"
