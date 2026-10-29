package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuiltInPriceFallback(t *testing.T) {
	// Mock CoinPaprika: id -> price. The app must use it when
	// PRICE_API_URL is unset, and must map L2 coins to ETH.
	prices := map[string]float64{
		"btc-bitcoin": 50000, "eth-ethereum": 2500, "xmr-monero": 600,
		"usdt-tether": 1.0, "matic-polygon": 0.1,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id string
		fmt.Sscanf(r.URL.Path, "/v1/tickers/%s", &id)
		p, ok := prices[id]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"not found"}`)
			return
		}
		fmt.Fprintf(w, `{"symbol":"X","quotes":{"USD":{"price":%v}}}`, p)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.PriceBaseURL = srv.URL
	cases := []struct {
		coin string
		want float64
	}{
		{"BTC", 50000}, {"ETH", 2500}, {"ARB", 2500}, {"OP", 2500}, {"BASE", 2500},
		{"XMR", 600}, {"USDT", 1.0}, {"MATIC", 0.1},
	}
	for _, c := range cases {
		got, err := app.fetchPriceUSD(c.coin)
		if err != nil || got != c.want {
			t.Errorf("price %s = %v err %v, want %v", c.coin, got, err, c.want)
		}
	}
	// Legacy PRICE_API_URL still wins when set.
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"price_usd": 12345}`)
	}))
	defer legacy.Close()
	app.cfg.PriceAPIURL = legacy.URL
	if got, err := app.fetchPriceUSD("BTC"); err != nil || got != 12345 {
		t.Errorf("legacy price = %v err %v, want 12345", got, err)
	}
	// Bad prices never become invoice amounts: zero, negative, and
	// non-JSON responses all error instead of dividing by zero.
	for _, body := range []string{`{"price_usd": 0}`, `{"price_usd": -5}`, `not json`, ``} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		app.cfg.PriceAPIURL = bad.URL
		if _, err := app.fetchPriceUSD("BTC"); err == nil {
			t.Errorf("price body %q should error", body)
		}
		bad.Close()
	}
}

func TestLegacyPriceSymbolMapping(t *testing.T) {
	// A legacy price API (cryptoPriceAPI) must be asked for ETH when
	// cryptopay creates an ARB invoice, because ARB pays in ETH on
	// Arbitrum, not in the ARB token. A server that only answers ETH
	// proves the mapping, not the test's assumption.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/price/ETH" {
			fmt.Fprint(w, `{"price_usd": 2500}`)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":"no price data"}`)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.PriceAPIURL = srv.URL
	if got, err := app.fetchPriceUSD("ARB"); err != nil || got != 2500 {
		t.Fatalf("ARB legacy price = %v err %v, want 2500 via ETH", got, err)
	}
	if gotPath != "/price/ETH" {
		t.Fatalf("ARB legacy request path = %q, want /price/ETH", gotPath)
	}
	// Trailing slash on the base URL must not double the slash.
	app.cfg.PriceAPIURL = srv.URL + "/"
	gotPath = ""
	if _, err := app.fetchPriceUSD("OP"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/price/ETH" {
		t.Fatalf("OP legacy request path = %q, want /price/ETH", gotPath)
	}
}

func TestDefaultPort(t *testing.T) {
	t.Setenv("API_TOKEN", "test-token-for-port-check")
	t.Setenv("PORT", "")
	cfg := loadConfig()
	if cfg.Port != "18777" {
		t.Fatalf("default PORT = %q, want 18777 (uncommon, so it does not clash with cryptoPriceAPI or a web server on 8080)", cfg.Port)
	}
}

func TestCreateInvoiceValidation(t *testing.T) {
	app := testApp(t)
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  createInvoiceReq
	}{
		{"unknown coin", createInvoiceReq{Coin: "BNB", AmountCrypto: 1}},
		{"both amounts", createInvoiceReq{Coin: "BTC", AmountCrypto: 1, AmountUSD: 10}},
		{"no amount", createInvoiceReq{Coin: "BTC"}},
		{"negative amount", createInvoiceReq{Coin: "BTC", AmountCrypto: -1}},
		{"huge amount", createInvoiceReq{Coin: "BTC", AmountCrypto: 2e9}},
		{"javascript webhook", createInvoiceReq{Coin: "BTC", AmountCrypto: 1, WebhookURL: "javascript:alert(1)"}},
		{"data return", createInvoiceReq{Coin: "BTC", AmountCrypto: 1, ReturnURL: "data:text/html,hi"}},
		{"long merchant ref", createInvoiceReq{Coin: "BTC", AmountCrypto: 1, MerchantRef: string(make([]byte, 300))}},
	}
	for _, c := range cases {
		if _, err := app.createInvoice(c.req); err == nil {
			t.Errorf("%s: should error", c.name)
		}
	}
	// A valid invoice still works.
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01, MerchantRef: "order-9", WebhookURL: "https://example.com/hook", ReturnURL: "https://example.com/thanks"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Address != testBTCAddr || inv.State != "awaiting" {
		t.Fatalf("valid invoice = %+v", inv)
	}
	// A second BTC invoice now succeeds on the shared address, with
	// a different quoted amount, instead of failing pool-exhausted.
	inv2, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatalf("second BTC invoice should reuse the shared address: %v", err)
	}
	if inv2.Address != testBTCAddr || inv2.AmountCrypto == inv.AmountCrypto {
		t.Fatalf("second invoice = %+v, want shared address and different amount", inv2)
	}
}
