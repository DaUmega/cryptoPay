package main

// cryptopay: a minimalist standalone crypto payment service.
//
// Model (the middle ground, not the full gateway):
//   - No private keys, no address derivation, no forwarding. The service
//     only watches addresses. Funds are swept manually by the operator.
//   - Each invoice gets a fresh address from a pre-generated pool, plus a
//     tiny random offset on the amount so quoted amounts are unique.
//   - The service tracks detecting, deposit detected, waiting on
//     confirmations, confirmed, then fires a webhook and lets the hosted
//     pay page redirect the customer back to the merchant.
//   - Storage is JSON files. Dependencies: none (Go standard library only).
//   - One port, one binary. TLS is the reverse proxy's job (or run it on a
//     private network, like a local price API).
//   - No third-party API keys at all. Every chain API below is a keyless
//     public endpoint (or, for XMR, the operator's own wallet RPC).
//     The only secrets are the operator's own: API_TOKEN for invoice
//     creation and pool admin, and WEBHOOK_SECRET for webhook HMAC.

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port          string
	Bind          string
	DataDir       string
	APIToken      string
	WebhookSecret string
	PriceAPIURL   string
	PriceBaseURL  string
	ExpiryMin     int
	PollSec       int
	PublicURL     string
	BTCExplorer   string
	LTCExplorer   string
	ETHAPI        string
	MATICAPI      string
	ARBAPI        string
	OPAPI         string
	BASEAPI       string
	AVAXAPI       string
	DOGEBlockbook string
	BCHBlockbook  string
	DASHBlockbook string
	ZECBlockbook  string
	AdaStatURL    string
	KoiosURL      string
	XRPNode       string
	XLMHorizon    string
	XMRWalletRPC  string
	XMRWalletUser string
	XMRWalletPass string
	TronAPI       string
	SolRPC        string
	USDCMint      string
	USDTMint      string
	Conf          map[string]int
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func loadConfig() *Config {
	token := os.Getenv("API_TOKEN")
	if token == "" {
		log.Fatal("API_TOKEN is required (generate one with: openssl rand -hex 32)")
	}
	conf := map[string]int{
		"BTC": envInt("CONF_BTC", 2), "LTC": envInt("CONF_LTC", 6),
		"ETH": envInt("CONF_ETH", 12), "MATIC": envInt("CONF_MATIC", 64),
		"ARB": envInt("CONF_ARB", 12), "OP": envInt("CONF_OP", 12),
		"BASE": envInt("CONF_BASE", 12), "AVAX": envInt("CONF_AVAX", 12),
		"DOGE": envInt("CONF_DOGE", 6), "BCH": envInt("CONF_BCH", 6),
		"DASH": envInt("CONF_DASH", 6), "ZEC": envInt("CONF_ZEC", 12),
		"ADA": envInt("CONF_ADA", 15), "XRP": envInt("CONF_XRP", 1),
		"XLM": envInt("CONF_XLM", 1), "TRX": envInt("CONF_TRX", 20),
		"SOL": envInt("CONF_SOL", 32), "USDC": envInt("CONF_USDC", 32),
		"USDT": envInt("CONF_USDT", 32), "XMR": envInt("CONF_XMR", 10),
	}
	return &Config{
		Port: env("PORT", "18777"), Bind: env("BIND", "0.0.0.0"),
		DataDir: env("DATA_DIR", "./data"), APIToken: token,
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		PriceAPIURL:   os.Getenv("PRICE_API_URL"),
		PriceBaseURL:  env("PRICE_API_BASE_URL", "https://api.coinpaprika.com"),
		ExpiryMin:     envInt("INVOICE_EXPIRY_MIN", 60),
		PollSec:       envInt("POLL_INTERVAL_SEC", 10),
		PublicURL:     strings.TrimSuffix(os.Getenv("PUBLIC_URL"), "/"),
		BTCExplorer:   env("BTC_ESPLORA_URL", "https://blockstream.info/api"),
		LTCExplorer:   env("LTC_ESPLORA_URL", "https://litecoinspace.org/api"),
		ETHAPI:        env("ETH_API_URL", "https://eth.blockscout.com/api"),
		MATICAPI:      env("MATIC_API_URL", "https://polygon.blockscout.com/api"),
		ARBAPI:        env("ARB_API_URL", "https://arbitrum.blockscout.com/api"),
		OPAPI:         env("OP_API_URL", "https://optimism.blockscout.com/api"),
		BASEAPI:       env("BASE_API_URL", "https://base.blockscout.com/api"),
		AVAXAPI:       env("AVAX_API_URL", "https://api.routescan.io/v2/network/mainnet/evm/43114/etherscan/api"),
		DOGEBlockbook: env("DOGE_BLOCKBOOK_URL", "https://blockbook.doge.zelcore.io"),
		BCHBlockbook:  env("BCH_BLOCKBOOK_URL", "https://blockbook.bch.zelcore.io"),
		DASHBlockbook: env("DASH_BLOCKBOOK_URL", "https://blockbook.dash.zelcore.io"),
		ZECBlockbook:  env("ZEC_BLOCKBOOK_URL", "https://blockbook.zec.zelcore.io"),
		AdaStatURL:    env("ADA_ADASTAT_URL", "https://api.adastat.net/rest/v1"),
		KoiosURL:      env("ADA_KOIOS_URL", "https://api.koios.rest/api/v1"),
		XRPNode:       env("XRP_RPC_URL", "https://xrplcluster.com"),
		XLMHorizon:    env("XLM_HORIZON_URL", "https://horizon.stellar.org"),
		XMRWalletRPC:  env("XMR_WALLET_RPC_URL", "http://127.0.0.1:18088/json_rpc"),
		XMRWalletUser: os.Getenv("XMR_WALLET_RPC_USER"),
		XMRWalletPass: os.Getenv("XMR_WALLET_RPC_PASS"),
		TronAPI:       env("TRON_API_URL", "https://api.trongrid.io"),
		SolRPC:        env("SOL_RPC_URL", "https://api.mainnet-beta.solana.com"),
		USDCMint:      env("USDC_MINT", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
		USDTMint:      env("USDT_MINT", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"),
		Conf:          conf,
	}
}

type App struct {
	cfg    *Config
	store  *Store
	xmrMu  sync.Mutex
	xmrIdx map[string][2]uint64 // pool address to (account, subaddress) index
}

// required returns the configured confirmations for a coin, defaulting to
// 1 when unconfigured. A missing entry must never mean 0, or any payment
// would instantly count as confirmed.
func (c *Config) required(coin string) int {
	if n, ok := c.Conf[coin]; ok && n > 0 {
		return n
	}
	return 1
}

func (a *App) requireToken(w http.ResponseWriter, r *http.Request) bool {
	if a.cfg.APIToken == "" {
		writeErr(w, 401, "unauthorized")
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.APIToken)) != 1 {
		writeErr(w, 401, "unauthorized")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	a.store.mu.Lock()
	pool := map[string]map[string]int{}
	for coin, entries := range a.store.Pool {
		c := map[string]int{"available": 0, "assigned": 0, "retired": 0}
		for _, e := range entries {
			c[e.Status]++
		}
		pool[coin] = c
	}
	a.store.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "coins": acceptedCoins, "pool": pool})
}

type createInvoiceReq struct {
	Coin         string  `json:"coin"`
	AmountUSD    float64 `json:"amount_usd"`
	AmountCrypto float64 `json:"amount_crypto"`
	MerchantRef  string  `json:"merchant_ref"`
	WebhookURL   string  `json:"webhook_url"`
	ReturnURL    string  `json:"return_url"`
}

// decodeBody reads a JSON request body with a 64KB cap. Without the cap
// a client could send an unbounded body and grow the server's memory.
func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(out); err != nil {
		writeErr(w, 400, "bad json")
		return false
	}
	return true
}

func (a *App) handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if !a.requireToken(w, r) {
		return
	}
	var req createInvoiceReq
	if !decodeBody(w, r, &req) {
		return
	}
	inv, err := a.createInvoice(req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, invoiceResponse(a.cfg, inv))
}

func (a *App) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/invoices/")
	if !validInvoiceID(id) {
		writeErr(w, 404, "not found")
		return
	}
	a.store.mu.Lock()
	inv := a.store.Invoices[id]
	a.store.mu.Unlock()
	if inv == nil {
		writeErr(w, 404, "invoice not found")
		return
	}
	writeJSON(w, 200, invoiceResponse(a.cfg, inv))
}

type poolAddReq struct {
	Coin      string   `json:"coin"`
	Addresses []string `json:"addresses"`
}

func (a *App) handlePool(w http.ResponseWriter, r *http.Request) {
	if !a.requireToken(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req poolAddReq
		if !decodeBody(w, r, &req) {
			return
		}
		added, skipped, err := a.store.addToPool(req.Coin, req.Addresses)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"added": added, "skipped": skipped, "pool": a.store.poolCounts(req.Coin)})
	case http.MethodGet:
		a.store.mu.Lock()
		out := map[string][]*PoolEntry{}
		for coin, entries := range a.store.Pool {
			out[coin] = entries
		}
		a.store.mu.Unlock()
		writeJSON(w, 200, out)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (a *App) handlePayPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/pay/")
	if !validInvoiceID(id) {
		http.NotFound(w, r)
		return
	}
	a.store.mu.Lock()
	inv := a.store.Invoices[id]
	a.store.mu.Unlock()
	if inv == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = payTemplate.Execute(w, payPageData(a.cfg, inv))
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/invoices", a.handleCreateInvoice)
	mux.HandleFunc("/invoices/", a.handleGetInvoice)
	mux.HandleFunc("/pay/", a.handlePayPage)
	mux.HandleFunc("/admin/pool", a.handlePool)
	return mux
}

func main() {
	cfg := loadConfig()
	if cfg.PollSec <= 0 {
		cfg.PollSec = 10
	}
	if cfg.ExpiryMin <= 0 {
		cfg.ExpiryMin = 60
	}
	store, err := loadStore(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{cfg: cfg, store: store, xmrIdx: map[string][2]uint64{}}
	go app.watchLoop()
	limiter := newRateLimiter(120, 30)
	srv := &http.Server{
		Addr:              cfg.Bind + ":" + cfg.Port,
		Handler:           limiter.middleware(app.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("cryptopay listening on %s:%s (data %s)", cfg.Bind, cfg.Port, cfg.DataDir)
	log.Fatal(srv.ListenAndServe())
}
