# cryptopay

A minimalist standalone crypto payment service. One binary, one port,
zero dependencies (Go standard library only), JSON files for storage.
Any website can hook onto it for payments.

## The model (and what it deliberately does not do)

This is the middle ground between a single reused address and a full
payment gateway:

- **Shared address, amount-distinguished invoices.** Each invoice
  gets an address from a pre-generated pool, and when the pool has
  no fresh address left for a coin, invoices reuse an assigned one
  instead of failing. Concurrent invoices on one address are
  distinguished by their quoted amounts, not by address alone, so
  one address per coin is enough to keep selling, with a clean
  per-order trail (invoice to amount to transaction) for support,
  refunds, and tax. ADA shares too, distinguished by balance above a baseline
  recorded at creation, since its lister returns the whole
  balance, not per-transaction amounts.
- **Tiny random offset.** The quoted amount is the base amount plus a
  random 1 to 99 base units (satoshis, gwei, lovelace, drops, sun,
  lamports, micro-units). Quoted amounts are unique among open
  invoices for a coin, and stay reserved for expired invoices
  inside their 7-day late-payment window, so no two invoices can
  quote the same amount and claim each other's payment.
- **Confirmation tracking.** Invoices walk: awaiting (detecting),
  detected (payment seen, 0 confirmations), confirming (1 to N-1),
  confirmed (N reached). Required confirmations are configurable per
  coin. On confirmation it fires a webhook and the hosted pay page
  redirects the customer to the merchant's return URL.
- **No private keys, no derivation, no forwarding.** The service only
  watches addresses. It cannot spend anything, so there is nothing to
  steal from it. Funds are swept manually by the operator. That is the
  whole security pitch: the two hardest, riskiest parts of a full
  gateway (key derivation and transaction signing) are simply absent.
  The cost is that forwarding is manual, and the operator must keep
  the pool topped up.

## Quick start (turnkey setup, recommended for non-technical operators)

```bash
./scripts/setup.sh
```

That one command generates your API token and webhook secret,
asks a few plain questions (port, whether your website is on the
same machine, whether you already run cryptoPriceAPI, an optional
pay page address, optional Monero), writes `.env` with mode 600,
starts the service with Docker, and helps you add pool addresses.
It then explains webhooks in plain language and prints a
production checklist (backups, pool top-ups, updates). It never
creates a Monero wallet, never touches a seed, and never
overwrites an existing `.env` without asking (it backs it up
first). Run it again any time to check the service or top up
pools. For automation (for example, calling it from another
repo), use `./scripts/setup.sh --non-interactive` with
`SETUP_PORT`, `SETUP_PRICE_API_URL`, `SETUP_PUBLIC_URL`,
`SETUP_LOCAL_BIND`, and `SETUP_START` in the environment; in
that mode it only writes `.env` and starts Docker if asked.

## Quick start (manual)

```bash
cp .env.example .env
# Edit .env: set API_TOKEN (openssl rand -hex 32), and PRICE_API_URL if
# you want to create invoices by USD amount.
docker compose up -d --build
curl http://127.0.0.1:18777/healthz
```

Or run the binary directly (Go 1.23+):

```bash
go build -o cryptopay .
API_TOKEN=... PORT=18777 DATA_DIR=./data ./cryptopay
```

The service has no TLS. Run it on localhost or a private network, or
put it behind a reverse proxy (Caddy, nginx) for TLS, the same way a
local price API would be run. The compose file maps
`127.0.0.1:18777:18777` by default, so it is localhost only. It also
includes a `host.docker.internal` mapping, so a price API running on
the host is reachable from inside the container. If your
shop runs on a different machine, change that line to `18777:18777`
and put it behind a firewall or reverse proxy first. Do not expose it to the internet bare:
invoice creation and pool admin need the API token, but the pay page
and invoice status are public by design (by unguessable UUID).

## Configuration

All configuration is environment variables; see `.env.example` for the
full list with defaults. The only required one is `API_TOKEN`. The
important ones:

| Variable | Default | Notes |
|---|---|---|
| `PORT` | 18777 | Deliberately uncommon, so it does not clash with a web server or cryptoPriceAPI on 8080. Keep this value in Docker (the HEALTHCHECK uses it); map a different host port in compose if needed. |
| `BIND` | 0.0.0.0 | Use 127.0.0.1 when running bare locally. |
| `DATA_DIR` | ./data (/data in Docker) | invoices.json, pool.json. Back this up. |
| `API_TOKEN` | (required) | Bearer token for invoice creation and pool admin. |
| `WEBHOOK_SECRET` | (unset) | HMAC secret for webhook signatures. Unset means unsigned webhooks (testing only). |
| `PRICE_API_URL` | (unset) | Your own cryptoPriceAPI (recommended, see below), in the same shape: GET `<base>/price/<SYMBOL>` returns `{"price_usd": number}`. Only needed for `amount_usd` invoices; leave unset to use the built-in CoinPaprika fallback. |
| `INVOICE_EXPIRY_MIN` | 60 | Invoice lifetime in minutes. |
| `POLL_INTERVAL_SEC` | 10 | Watch loop interval, matching the web frontend's 10s poll. Raise it if you have many open invoices or hit API rate limits. |
| `PUBLIC_URL` | (unset) | Base URL prepended to pay links in API responses. Without it, `pay_url` is a relative path. |
| `CONF_<COIN>` | BTC 2, LTC 6, ETH 12, MATIC 64, ARB 12, OP 12, BASE 12, AVAX 12, DOGE 6, BCH 6, DASH 6, ZEC 12, ADA 15, XRP 1, XLM 1, TRX 20, SOL 32, USDC 32, USDT 32, XMR 10 | Required confirmations per coin. |

Per-coin API endpoints are also configurable (see `.env.example`).
Defaults are keyless public endpoints, and there are no API-key
variables at all: Esplora (BTC, LTC), Blockscout (ETH, MATIC, ARB,
OP, BASE), Routescan (AVAX), Zelcore Blockbook (DOGE, BCH, DASH,
ZEC), AdaStat plus Koios (ADA), xrplcluster (XRP), Stellar Horizon
(XLM), Trongrid (TRX), Solana public RPC (SOL, USDC, USDT). USDC
and USDT here mean Solana SPL tokens (mints configurable). BNB was
removed: it has no keyless Blockscout instance, and its only
explorer route is a keyed Etherscan API, which is exactly what this
service no longer depends on.

## Working together with cryptoPriceAPI

cryptopay does not fetch prices from exchanges itself. It has two
keyless price sources, in order:

1. **Your own cryptoPriceAPI (recommended).** That service
   ([cryptoPriceAPI](https://github.com/DaUmega/cryptoPriceAPI))
   aggregates live tickers from Binance, Kraken, Coinbase, Bybit,
   OKX, and KuCoin over public websockets, no keys, and returns
   one VWAP price with outlier filtering at `GET /price/<SYMBOL>`,
   which is exactly the shape cryptopay's `PRICE_API_URL` expects
   (`{"price_usd": number}`). Run it on its own port, because it
   also defaults to 8080, and that port is already taken on many
   hosts:

   ```bash
   git clone https://github.com/DaUmega/cryptoPriceAPI
   cd cryptoPriceAPI
   CRYPTOAPI_PORT=19080 ./manage.sh start
   curl http://127.0.0.1:19080/health
   ```

   Then point cryptopay at it:

   ```bash
   PRICE_API_URL=http://127.0.0.1:19080
   ```

   Two behaviours to know about, both from cryptoPriceAPI itself:
   the first `amount_usd` invoice for a coin can take up to about
   5 seconds, while cryptoPriceAPI subscribes to that symbol and
   waits for its exchanges to report; later invoices for the same
   coin are fast, because the price is already streaming. And a
   coin cryptoPriceAPI has no exchange data for returns 404, which
   cryptopay turns into a clear price error on invoice creation,
   never a guessed price.
2. **Built-in CoinPaprika fallback.** Leave `PRICE_API_URL` empty
   and cryptopay calls CoinPaprika directly, keyless, for the
   same coins. This is the out-of-the-box path when you do not
   run cryptoPriceAPI, and it is what the tests cover with a mock
   server. It is a single source, not a VWAP across exchanges,
   which is why the local aggregator is the recommended path for
   a real shop.

In both cases, ARB, OP, and BASE are priced as ETH, because they
pay in ETH on their L2. cryptopay does the mapping before it
asks either source, so the two sources agree.

## API

Auth: `Authorization: Bearer <API_TOKEN>` for POST /invoices and
/admin/pool. GET /healthz, GET /invoices/{id}, and GET /pay/{id} are
public (invoice IDs are random 32-hex UUIDs).

### Create an invoice

```bash
curl -X POST http://127.0.0.1:18777/invoices \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"coin":"BTC","amount_usd":30,"merchant_ref":"order-123",
       "webhook_url":"https://example.com/webhook/payments",
       "return_url":"https://example.com/thank-you"}'
```

Pass `amount_crypto` instead of `amount_usd` to skip the price API.
Response (201):

```json
{
  "id": "12b48a2d0d42045c237d376abba65217",
  "coin": "BTC",
  "address": "bc1q...",
  "amount_crypto": 0.01000062,
  "state": "awaiting",
  "confirmations": 0,
  "confirmations_required": 2,
  "payment_uri": "bitcoin:bc1q...?amount=0.01000062",
  "pay_path": "/pay/12b48a2d0d42045c237d376abba65217",
  "pay_url": "/pay/12b48a2d0d42045c237d376abba65217",
  "expires_at": "2026-10-27T01:04:54-06:00"
}
```

The merchant either redirects the customer to `pay_url` (the hosted
pay page, with address, amount, and live status), or renders
its own payment UI from `address`, `amount_crypto`, and
`payment_uri` (and renders its own QR code from `payment_uri` if it
wants one; the hosted page deliberately loads no third-party QR
image, so no payment address or amount is ever sent to a QR API
operator).

### Check an invoice

```bash
curl http://127.0.0.1:18777/invoices/12b48a2d0d42045c237d376abba65217
```

Returns the same shape plus `received`, `confirmed_received`,
`txids`, `webhook_status`, and the current `state`.

### Pool admin

```bash
# Add addresses (any time, to top up):
curl -X POST http://127.0.0.1:18777/admin/pool \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"coin":"BTC","addresses":["bc1q...","bc1q..."]}'
# Or from a file (one address per line, # comments skipped):
PAYMENTS_URL=http://127.0.0.1:18777 API_TOKEN=... ./scripts/add-pool.sh BTC addresses.txt
# List the pool (counts and statuses):
curl http://127.0.0.1:18777/admin/pool -H "Authorization: Bearer $API_TOKEN"
# Health plus pool counts (for monitoring and setup preflights):
curl http://127.0.0.1:18777/healthz
```

## The address pool: generating and managing it

The pool is the operator's job, and it is deliberately manual: you
generate addresses with your own wallet software (nothing here ever
sees a seed or private key), then hand them to the service.

**Generating a batch per coin.** Use whatever wallet you already
trust, create new receiving addresses, and copy them out:

- BTC: Bitcoin Core (`bitcoin-cli getnewaddress` in a loop), or
  Electrum (it generates receiving addresses on demand; export them
  from the Addresses tab, or `electrum listaddresses --receiving`).
- LTC: same as BTC (Litecoin Core, or Electrum-LTC). LTC reuses the
  BTC Esplora lister with a different base URL, so nothing else changes.
- ETH: MetaMask (create accounts, copy the addresses), or any
  wallet that lets you add accounts.
- MATIC, ARB, OP, BASE, AVAX: the same MetaMask (or any EVM wallet)
  accounts work on every EVM chain, since the address format is
  identical. One address per invoice per chain is still required,
  because the pool is per coin: the same address can sit in the
  MATIC pool and the ARB pool, but an invoice draws from one coin's
  pool only, so keep the pools topped up separately. No key is
  needed for any of them (Blockscout for MATIC/ARB/OP/BASE,
  Routescan for AVAX). ARB, OP, and BASE pay in native ETH on
  their L2, not in ARB/OP tokens, and are priced as ETH (see the
  price section). BNB is not accepted any more: no keyless
  explorer covers it.
- ADA: Daedalus or Yoroi (generate new receiving addresses).
- XRP: Xaman or any XRP wallet. See the activation note below.
- XLM: any Stellar wallet (Freighter, Lobstr, etc.). No activation
  reserve is needed the way XRP needs one for a first receive, but
  a brand-new Stellar account still needs its 1 XLM minimum
  balance funded before it can receive, so fund pool addresses
  once, the same way you activate XRP addresses.
- TRX: TronLink (create accounts, copy the addresses).
- SOL, USDC, and USDT (Solana): Phantom or Solflare (create
  accounts; the same address receives SOL and both SPL stablecoins).
  USDT here is the real Solana mint
  `Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB`, not the
  lookalike mints that also start with `Es9v`.
- DOGE, BCH, DASH, ZEC: any wallet for the coin, via Zelcore
  Blockbook (keyless, default). BCH pool addresses must be
  cashaddr with the `bitcoincash:` prefix (bare cashaddr is
  rejected by Blockbook, and legacy addresses are normalised to
  cashaddr, so accepting them would create invoices Blockbook
  could not match). ZEC pool addresses must be transparent
  (t1/t3); shielded addresses cannot be watched and are rejected
  when added to the pool.
- XMR: subaddresses from your Monero wallet (one per invoice for
  privacy, though invoices can share one, like other coins). See
  "Monero setup" below; it is a different flow from the other coins,
  and the repo teaches it step by step.

Start with one address per coin you accept; more addresses give
more privacy (no public revenue dossier on one address) but are
not required for correctness any more, since invoices share
addresses and are distinguished by amount (ADA by balance above
baseline). More ADA addresses are still nice for privacy, but
not required for correctness any more. The service logs nothing about low pools
except the counts endpoint, so check it on whatever cadence you
check your other services.

**Rules that keep the pool correct:**

- Never spend from a pool address until its invoice is confirmed.
  The service reads the address balance and transactions to detect
  payments; spending early confuses that (and for ADA, the balance
  is the payment signal). Sweep after confirmation, not before.
- The service itself reuses addresses for new invoices when no
  fresh address is left (except ADA). You do not need to re-add
  addresses by hand to keep selling. Expired and underpaid
  invoices retire their address unless another open invoice still
  uses it, and a retired address is reused automatically when
  needed; its quoted amount stays reserved for the 7-day
  late-payment window, so a late payment still belongs to the
  old invoice, not to a new one.
- Re-adding a retired address is an explicit operator decision
  (POST it again; the service flips it back to available). Only do
  that when you are confident no late payment is coming to it.
- XRP addresses must be activated before they can receive: an XRP
  account needs its 10 XRP reserve funded first (send 10 XRP to each
  pool address once). Until activation, the service sees no payments
  for that address (it treats an unactivated account as empty, not
  as an error). Other coins need no activation.
- Back up `DATA_DIR`. It holds the invoices and the pool. If it is
  lost, open invoices lose their address mapping (funds already
  sent are still on-chain and recoverable from the addresses, but
  the service will not know which invoice they belong to).

## Monero setup: the client supplies a cheap hot wallet

Monero does not work like the other coins, and the repo teaches the
setup instead of hiding it. The short version: the client (you, the
merchant) creates a dedicated Monero wallet just for this service, a
"cheap hot wallet" that holds only recent payments and gets swept
regularly, so even a full compromise costs at most the unswept
balance. The service itself never sees a seed, a spend key, or the
view key: it only talks to a view-only wallet RPC that you run.

**Why a wallet RPC and not a public API.** Monero's privacy means
no explorer can answer "did this subaddress receive funds" without
your view key, because outputs are not linkable to an address
on-chain. The wallet does the scanning locally with the view key
and reports transfers; cryptopay just asks it. And because the
wallet on the server is view-only, it cannot spend even if
compromised: the spend key stays in your full wallet, offline or on
your own machine.

**Integrated setup, guided (about 15 minutes, once, skippable):**

Run `./scripts/setup-monero.sh`, or answer yes to Monero in
`./scripts/setup.sh` Step 7. Skipping is fine: XMR is simply not
offered until you run setup-monero.sh later, and you never have to
redo the rest of setup. The script is guided, not fully automated,
because a human must supply the view key, choose a password, and
choose nodes, and a script should never handle a seed.

1. Create a dedicated full wallet first, if you do not have one
   yet: `monero-wallet-cli --generate-new-wallet cryptopay-hot`
   (or the GUI, or a mobile wallet like Monerujo). Write down the
   25-word seed and keep it offline. This wallet is only for
   cryptopay receipts; do not use it as your savings wallet.
   This script never creates a full wallet and never touches a
   seed, on purpose.
2. Give setup-monero.sh 1 to 10 nodes you chose yourself
   (monero.fail lists public ones with sync status). It tests
   each one and keeps only nodes that answer, are mainnet, and
   are synchronized. Prefer synced, low-latency nodes. Privacy
   note from Monero's own docs: a remote node can log your IP
   and the blocks you request, so prefer an onion or I2P node
   from that list if that matters to you. A remote node can hide
   payments from you, but it cannot forge a confirmed payment:
   the wallet verifies transactions cryptographically, and the
   RPC is never run with --trusted-daemon for a public node.
3. The script downloads and hash-verifies the Monero CLI for
   your arch (v0.18.5.4), then guides a view-only wallet
   generation from your full wallet: it prompts for a wallet
   password (hidden, written only to
   `monero-wallets/viewonly.pass`, mode 600), then runs
   `monero-wallet-cli --generate-from-view-key
   monero-wallets/viewonly`, which prompts you, interactively,
   for the main address and the secret view key (`address` and
   `viewkey` in the full wallet CLI, or Settings, then Show seed
   and keys, in the GUI). Those values are never logged, never
   written to .env, and never passed on a command line. The
   restore height defaults to the best node height minus 1000,
   which keeps the first sync to minutes instead of hours or
   days, and you can override it with your wallet creation
   height. This wallet can see payments but cannot spend.
4. The script starts monero-wallet-rpc as its own service in
   the same compose stack (`docker-compose.monero.yml`), one
   binary per container: cryptopay stays a single Go binary,
   and the RPC service is a second container, started only when
   Monero is enabled. Inside Docker, cryptopay reaches it as
   `http://monero-wallet-rpc:18088/json_rpc`; on the host, the
   port is published only on localhost
   (`127.0.0.1:18088`), so host tools can reach it while the
   public internet cannot. No RPC login in the compose service:
   it is reachable only from this host and the compose network.
   If you run an RPC elsewhere with --rpc-login instead, use
   `./scripts/setup-monero.sh --already-have-rpc` to record its
   URL and node list, and set XMR_WALLET_RPC_USER/PASS in .env.
5. Generate subaddresses from your full wallet, one per invoice
   (or a batch of 20 to 50 up front): `address new <label>` in
   monero-wallet-cli, or Receive, then Create new address in the
   GUI. Add them to the pool like any other coin
   (`POST /admin/pool` with coin XMR). The view-only wallet tracks
   every subaddress of the account automatically, so nothing else
   needs to be told about them.
6. Sweep regularly from the full wallet to cold storage. That is
   the whole "cheap hot wallet" idea: the hot wallet holds only
   what has arrived since the last sweep.

**Automatic daemon failover (optional, Feather-style).** Feather
picks a node at random from a built-in curated list; cryptopay
keeps the same idea but with an operator-curated list, because a
merchant should not let a third-party list server pick its node.
`scripts/xmr-node-watch.sh` is a one-shot watchdog, run it from
cron every 5 minutes with `XMR_NODE_LIST` set to 1 to 10 nodes you
picked from monero.fail. Each run probes every node's
`/get_height`, and if the daemon the wallet is on is down or more
than 10 blocks behind the best node in the list, it calls the
wallet RPC's `set_daemon` (trusted=false, never trusted for a
public node) to switch, and records the choice in a state file.
It never touches keys or the wallet file, and it never starts or
restarts wallet-rpc, so it needs no wallet password. With the
compose service, it needs no RPC login either, because the RPC
is localhost-only on the host; setup-monero.sh offers the cron
line with the host perspective URL
(`http://127.0.0.1:18088/json_rpc`), not the container service
name. If no node answers, it
exits non-zero and leaves the daemon alone, so a bad list cannot
switch you to a dead node. If you switch daemons by hand, delete
the state file so the next run re-anchors. Test it with
`./scripts/test-xmr-node-watch.sh` (mock daemons and mock wallet
RPC, 6 scenarios: healthy, lagging, down, all down, set_daemon
failure, empty list). Verified live 2026-10-28:
`node.monerodevs.org:18089` answered `/get_height` with height
3794294, while `node.supportxmr.com:18081` answered nothing,
which is exactly the case the watchdog exists for.

**Operational notes.** The wallet must be synced before payments
appear: if it is behind the chain tip, an XMR invoice stays
awaiting until the wallet catches up, which is correct behavior,
not a bug. If the public node goes down, point the wallet RPC at
another node from monero.fail and restart it; the wallet keeps its
local scan cache, so switching nodes is cheap. XMR confirmations
default to 10 blocks (the standard for unlocked outputs), and the
tiny random offset is 1 to 99 piconero (atomic units), which is
invisible in value but exact in the wallet's integer amounts, so
it still disambiguates invoices. Amounts in USD need your price
API to support XMR; otherwise create XMR invoices by amount_crypto.

## Payment flow, states, and edge cases

States: `awaiting` (detecting), `detected` (payment seen, 0
confirmations), `confirming` (1 to N-1), `confirmed` (N reached),
`expired` (no payment by expiry), `underpaid` (partial payment
still short 24h after expiry), `paid_late` (fully paid after
expiry).

- A payment is accepted when the confirmed total reaches the quoted
  amount within a 0.1% tolerance (for fee rounding). Overpayment
  counts as paid (the extra is a tip, not a failure). Partial
  payments accumulate: a top-up to the same address adds to the
  total, and the invoice confirms once the confirmed total reaches
  the quoted amount.
- Expiry only closes invoices with no payment, or with a partial
  payment that stays short for 24h past expiry. A detected payment
  that is still confirming never expires on its own: the money is
  real and on its way.
- Expired and underpaid invoices keep polling for 7 days after
  expiry, so a late payment is never silently ignored: if it fully
  pays, the invoice becomes `paid_late` and fires its webhook for
  manual review. After 7 days the service stops polling it (check
  the address on an explorer after that).
- ADA confirmations use Koios (keyless, tier-limited at 5,000
  requests per day per IP). Detection always works via AdaStat
  (keyless, exact balance), and Koios is only queried once a
  balance is seen, so empty invoices cost one AdaStat call per
  poll, not three Koios calls. If Koios does not answer, an ADA
  invoice stays `detected` (safe: it never counts as confirmed on
  a balance alone).
- XLM is final once listed by Horizon (about 5 seconds), like XRP,
  so a listed payment counts as confirmed regardless of the
  configured count.
- XRP is final once validated (about 4 seconds), so a validated
  transaction counts as confirmed regardless of the configured
  count. SOL and USDC use Solana commitment: `finalized` counts as
  deeply confirmed, `confirmed` as 1.

## Webhooks

On `confirmed` (and on `paid_late`), the service POSTs to the
invoice's `webhook_url`:

```json
{
  "invoice_id": "12b48a2d0d42045c237d376abba65217",
  "merchant_ref": "order-123",
  "coin": "BTC",
  "address": "bc1q...",
  "amount_crypto": 0.01000062,
  "state": "confirmed",
  "txids": ["..."],
  "confirmed_at": "2026-10-27T00:20:00-06:00"
}
```

If `WEBHOOK_SECRET` is set, the request carries
`X-Cryptopay-Signature: hex(HMAC-SHA256(secret, body))`. Verify it
on your side (Python example):

```python
import hmac, hashlib
sig = hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(sig, request.headers["X-Cryptopay-Signature"])
```

Delivery retries at 0s, 5s, 30s, 2min, then marks
`webhook_status: failed` on the invoice (visible via
GET /invoices/{id}); the merchant can then fulfill manually from
the invoice status. If no `webhook_url` was given, there is nothing
to deliver and the merchant polls the status endpoint instead.

## Integrating a site with it

If your site currently watches balances itself (a shared wallet
per coin, polling for a balance rise by exactly the quoted amount,
then fulfilling the order), moving that work into cryptopay leaves
your backend doing only your own business logic:

- Purchase endpoint: instead of watching a wallet, create a
  cryptopay invoice (coin, price as `amount_usd`, or an
  already-computed crypto amount as `amount_crypto`;
  `merchant_ref` is your order id; `webhook_url` points at your
  webhook receiver; `return_url` points at your post-payment
  page) and return the pay URL (or address plus amount) to the
  frontend. The frontend shows it or redirects to it.
- Webhook receiver: on `confirmed` (verify the HMAC first), run
  your existing fulfillment code keyed by `merchant_ref`. The
  customer can then get their download, account, or product the
  way they do today (your frontend polls your backend by order
  id, or the pay page redirects to `return_url`).
- Code you no longer need: per-coin balance watching, and coin
  price fetching for payment amounts (prices now come from
  cryptopay via `PRICE_API_URL`, or from your own price source
  if you already have one). Non-crypto payment methods stay
  entirely on your side; they never touch cryptopay.
- Setup preflight: your deploy script can call
  `scripts/web-preflight.sh` at the top, before building
  anything. It curls `$PAYMENTS_URL/healthz` and fails fast with
  start instructions when cryptopay is not up.

That is a production payment change, so do it on its own branch
with a compile pass and a live deployment test before the old
flow is removed.

## Security notes

- No keys, ever. Do not put a seed, private key, or wallet password
  in `.env`, in the repo, or in the container. The service does not
  want them and cannot use them.
- The API token is the only auth. Keep it out of the repo (`.env`
  is gitignored), rotate it if it leaks, and do not log it.
- Webhook URLs must be HTTPS in production, and the receiver must
  verify the HMAC, or anyone who guesses an invoice flow could fake
  a confirmation.
- Pool addresses are public by design (they are deposit addresses),
  but the pool list endpoint is token-gated anyway, so an attacker
  cannot enumerate your pool to grief it.
- Rate limits are the operator's problem at scale: the defaults
  are keyless public APIs, shared with everyone else on that IP.
  If you run many open invoices, raise `POLL_INTERVAL_SEC`, and
  point a chain's `*_URL` at a self-hosted explorer if the public
  instance challenges or throttles your IP. There is no key to
  set any more: a key would only move the rate limit, not remove
  it, and it would be a secret to leak and a single point of
  failure to DDoS.
- The server itself is rate-limited (120 requests per minute per
  IP, burst 30), request bodies are capped at 64KB, and server
  timeouts are set, so one client cannot exhaust memory or
  connections, or get the operator's IP throttled upstream by
  making cryptopay poll in a loop.
- Pool addresses are format-validated when added, and invoice
  fields are validated when created (amounts, URL schemes, ref
  length), so a typo or a `javascript:` URL is a clear 400, not
  an invoice that can never be paid or a pay page that redirects
  somewhere it should not.

## What was tested

- `go vet` clean, `gofmt` clean, 50 tests pass (`go test ./...`):
  unit tests (state machine, offset uniqueness, pool add/retire/
  re-add, payment URIs, webhook HMAC, Tron base58, address
  validation for every coin, URL validation, invoice-ID
  validation, rate limiter, corrupt-store fail-closed) plus
  mock-API integration tests (a fake Esplora walks a BTC invoice
  from awaiting through detected to confirmed and fires a webhook;
  the same mock walks an LTC invoice, proving the Esplora reuse;
  a fake Blockscout server proves an EVM chain works with no key
  and confirms at the chain's threshold; fake Blockbook servers
  prove DOGE/BCH/DASH/ZEC parsing and the clear error when no
  Blockbook URL is set; a fake Horizon proves XLM native-payment
  filtering and finality; a fake Solana RPC proves USDC/USDT token
  payments and the no-token-account case; a fake view-only wallet
  RPC proves the XMR flow, including the subaddress index cache
  and the mempool plus confirmed split; fake price APIs prove
  USD conversion via a legacy price API and via the built-in
  CoinPaprika fallback, including the ARB/OP/BASE-to-ETH price
  mapping; bogus payments (negative, zero, empty txid) never
  confirm; an ADA top-up after the first poll is not lost;
  three slow listers finish in parallel, not serially; a lister
  error leaves the invoice untouched; empty-pool errors; expiry
  and late payment; the full HTTP API including auth, method
  checks, oversized bodies, token-gated pool listing, and the
  pay page with no third-party QR image; production regressions
  (seller-address history never confirming a new invoice, ETH
  lister 429 failing safe, ADA confirming only on Koios depth,
  Solana baselines using signature lists only, expiry and
  late-window amount reservation, baseline claiming for every
  coin).
- Store rollback on save failure: pool adds, address assignment,
  and invoice creation all leave the in-memory state exactly as
  the API reported it when the disk save fails (a failed pool add
  used to appear in the pool anyway, then vanish on restart).
- The turnkey setup: `./scripts/test-setup.sh` passes
  (non-interactive .env generation, mode 600, 64-hex secrets,
  idempotency, invalid port rejection), and the full interactive
  setup was run on Kali with real Docker (see below).
- The XMR watchdog: `./scripts/test-xmr-node-watch.sh` passes 6
  mock scenarios (healthy no-switch, lagging switch, down switch,
  all-down no-switch, set_daemon failure keeps state, empty list
  refuses), and a live `/get_height` probe against a real public
  node returned height 3794294 in the expected shape.
- The Monero setup: `./scripts/test-setup-monero.sh` passes
  (syntax, node validation against mock daemons: good mainnet
  synced passes, unsynced, testnet, and down nodes rejected,
  first good node picked, --already-have-rpc writes .env,
  11 nodes rejected, non-interactive wallet generation refused).
  The full guided wallet generation and RPC service start are
  not covered by that test, because they need a human view key
  and Docker; they are tested by running setup-monero.sh
  interactively on a test host, not by a mock.
- A live binary smoke test (no chain calls): healthz, pool add,
  invoice create (0.01 BTC quoted as 0.01000062 with a 62-sat
  offset), invoice fetch, pay page render, pool list.
- Also verified live on Kali (2026-10-28): `go test ./...` passes
  there too, the Docker image builds and runs (healthz on 18777
  with the 20-coin list), and cryptopay works end to end with a
  real cryptoPriceAPI running on the same machine (port 19080):
  a $100 BTC invoice priced at 0.001233 BTC and a $100 ARB
  invoice at 0.04246 ETH, proving the ARB-to-ETH price mapping
  against live exchange data, not just mocks. The turnkey setup
  was also run end to end on Kali: it generated secrets, bound
  the port to localhost, started Docker, added BTC and ETH pool
  addresses, and those addresses survived a container restart
  (pool.json on disk, owned by nobody), which is what caught the
  two bugs fixed above.
- Not tested here, honestly: no real-funds end-to-end payment was made: the first real payment
  through it (any coin) is the confirming step, and it should be a
  small amount you control. That applies doubly to XMR: the wallet
  RPC flow is mock-tested only, so the first XMR payment should be
  a small self-controlled one to a pool subaddress, confirming the
  wallet RPC sees it and the invoice confirms at 10 blocks.

## Adding another coin

The pattern is deliberately small, and it is the same three steps
every time:

- Bitcoin-like chain with an Esplora-compatible API (LTC is the
  example): one dispatch line pointing at the BTC lister with a
  different base URL, one offset unit, one payment URI scheme, one
  confirmations default. About 10 lines.
- EVM chain (MATIC, ARB, OP, BASE are the Blockscout examples,
  AVAX is the Routescan example): one dispatch line pointing at
  the ETH lister with that chain's keyless base URL, plus the
  offset (1 gwei) and the ethereum URI scheme. About 10 lines.
  Do not add a chain whose only explorer route is keyed: that
  is why BNB is gone, and the same rule applies to the next one.
- Solana SPL token (USDT is the example): one dispatch line
  pointing at the USDC lister with a different mint. About 5 lines.
- Blockbook coin (DOGE, BCH, DASH, ZEC are the examples): one
  dispatch line pointing at the Blockbook lister with that coin's
  Zelcore URL. About 5 lines, plus a validator entry.
- Anything else: write one lister function (50 to 100 lines) that
  returns payments as (txid, amount in whole coins, confirmations),
  add it to the dispatch, and set its offset unit, URI, and
  confirmations default. DOGE (Blockbook) and XMR (wallet RPC) are
  the examples of this shape.

## Layout

```
main.go       config, routing, handlers
store.go      JSON store, address pool
invoice.go    creation, offset, payment URIs, response shape
chains.go     per-coin payment listing (BTC, LTC, ETH, EVM chains, AVAX, DOGE, BCH, DASH, ZEC, ADA, XRP, XLM, TRX, SOL, USDC, USDT)
validate.go   address and URL validation, invoice-ID validation
limits.go     per-IP rate limiter for the HTTP server
xmr.go        Monero via a view-only wallet RPC (subaddress index cache)
price.go      price API client, HTTP helpers
webhook.go    HMAC webhook delivery with retries
paypage.go    hosted pay page (address, amount, live status, no third-party QR)
base58.go     base58 decode (Tron hex conversion)
watch.go      watch loop, expiry, late payments
scripts/      add-pool.sh, web-preflight.sh, xmr-node-watch.sh
              (XMR daemon failover) and its test harness
```
