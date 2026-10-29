package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Production regressions, Oct 2026: every bug that reached the live
// site gets a test here, so it cannot slip back in silently.
//
//  - Seller-address history false-confirming new invoices (the
//    critical bug: old payments summed to more than the new amount).
//  - A lister returning 429 / failing at creation (ETH Blockscout).
//  - ADA confirmations staying 0 when Koios returns nothing, and
//    confirming only when Koios actually reports depth.
//  - Solana baseline taking a full transaction listing (>30s, which
//    orphaned invoices at the web client's timeout).
//  - Expiry / late-payment rules, and quoted amounts staying reserved
//    through the 7-day late window.
//  - Baseline and exact-match-only claiming, for every coin's offset.

// A seller address with history: three old payments (including dust
// and an overpayment) that together exceed the new invoice. All of
// them are present at creation, so all must be baselined and ignored,
// and the new invoice must stay awaiting with received 0.
func TestSellerHistoryBatchNeverConfirms(t *testing.T) {
	txs := []struct {
		id     string
		amount int64
	}{
		{"old-1", 600000},
		{"old-dust", 100},
		{"old-big", 2000000},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/blocks/tip/height") {
			fmt.Fprint(w, "101")
			return
		}
		var out []map[string]any
		for _, tx := range txs {
			out = append(out, map[string]any{
				"txid":   tx.id,
				"status": map[string]any{"confirmed": true, "block_height": 100},
				"vout":   []map[string]any{{"scriptpubkey_address": testBTCAddr, "value": tx.amount}},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = srv.URL
	app.cfg.Conf["BTC"] = 2
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.IgnoreTxIDs) != len(txs) {
		t.Fatalf("baseline txids = %d, want %d (every old tx must be recorded)", len(inv.IgnoreTxIDs), len(txs))
	}
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "awaiting" || got.Received != 0 {
		t.Fatalf("seller history confirmed a new invoice: state %q received %v", got.State, got.Received)
	}
}

// Baseline claiming is coin-independent: an old txid in the baseline
// is never claimed, for any coin, even if its amount matches exactly.
// Without a baseline, only an exact match may claim, never a partial
// or an overpayment (that is how the seller-history bug happened).
func TestBaselineClaimingAllCoins(t *testing.T) {
	coins := []string{"BTC", "ETH", "XRP", "SOL", "USDC", "TRX", "ADA", "XMR"}
	for _, coin := range coins {
		app := testApp(t)
		inv := &Invoice{ID: "inv-" + coin, Coin: coin, Address: "shared-addr",
			AmountCrypto: 10, IgnoreTxIDs: []string{"old-tx"}, BaselineDone: true}
		app.store.Invoices[inv.ID] = inv
		if app.claimsPayment(inv, Payment{TxID: "old-tx", Amount: 10}, 0) {
			t.Errorf("%s: baseline txid was claimed", coin)
		}
		// Exact-match-only fallback: no baseline, partial and
		// overpayment must not claim, exact must.
		nb := &Invoice{ID: "nb-" + coin, Coin: coin, Address: "shared-addr",
			AmountCrypto: 10, BaselineDone: false}
		app.store.Invoices[nb.ID] = nb
		if app.claimsPayment(nb, Payment{TxID: "new-partial", Amount: 5}, 0) {
			t.Errorf("%s: no-baseline partial was claimed", coin)
		}
		if app.claimsPayment(nb, Payment{TxID: "new-over", Amount: 20}, 0) {
			t.Errorf("%s: no-baseline overpayment was claimed", coin)
		}
		if !app.claimsPayment(nb, Payment{TxID: "new-exact", Amount: 10}, 0) {
			t.Errorf("%s: no-baseline exact match was not claimed", coin)
		}
	}
}

// ETH Blockscout 429 at creation: the invoice must still be created
// in exact-match-only mode, must stay awaiting on the next poll, and
// must never confirm from the (unseen) old history.
func TestETHLister429IsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"message":"rate limited"}`)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.ETHAPI = srv.URL
	app.cfg.Conf["ETH"] = 12
	if _, _, err := app.store.addToPool("ETH", []string{testETHAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "ETH", AmountCrypto: 0.01})
	if err != nil {
		t.Fatalf("429 at creation must not block invoice creation: %v", err)
	}
	if inv.BaselineDone {
		t.Fatal("baseline must be false after a 429")
	}
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "awaiting" || got.Received != 0 {
		t.Fatalf("429 invoice: state %q received %v, want awaiting/0", got.State, got.Received)
	}
}

// ADA: a balance alone must never confirm. With Koios returning no
// txs, confirmations stay 0 and the invoice stays detected, even
// when the balance covers the amount. With Koios reporting depth
// >= required, the same balance confirms.
func TestADAConfirmationsNeedKoiosDepth(t *testing.T) {
	var bal int64
	var koiosDepth int // 0 = Koios returns nothing
	adaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ".json"):
			fmt.Fprintf(w, `{"data":{"balance":%d},"code":200}`, bal)
		case strings.HasSuffix(r.URL.Path, "/address_txs"):
			if koiosDepth == 0 {
				fmt.Fprint(w, `[]`)
			} else {
				fmt.Fprint(w, `[{"tx_hash":"ada-tx-1"}]`)
			}
		case strings.HasSuffix(r.URL.Path, "/tx_info"):
			fmt.Fprint(w, `[{"block_height":100}]`)
		case strings.HasSuffix(r.URL.Path, "/tip"):
			fmt.Fprintf(w, `[{"block_no":%d}]`, 100+koiosDepth-1)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer adaSrv.Close()
	addr := "addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"
	// Case 1: balance covers the amount, Koios silent -> detected, not confirmed.
	app := testApp(t)
	app.cfg.AdaStatURL = adaSrv.URL
	app.cfg.KoiosURL = adaSrv.URL
	app.cfg.Conf["ADA"] = 15
	if _, _, err := app.store.addToPool("ADA", []string{addr}); err != nil {
		t.Fatal(err)
	}
	bal = 0
	koiosDepth = 0
	inv, err := app.createInvoice(createInvoiceReq{Coin: "ADA", AmountCrypto: 5})
	if err != nil {
		t.Fatal(err)
	}
	bal = 5000000
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "detected" {
		t.Fatalf("ADA with no Koios depth: state %q, want detected (balance alone must not confirm)", got)
	}
	// Case 2: same setup, Koios reports 15 blocks of depth -> confirmed.
	app2 := testApp(t)
	app2.cfg.AdaStatURL = adaSrv.URL
	app2.cfg.KoiosURL = adaSrv.URL
	app2.cfg.Conf["ADA"] = 15
	if _, _, err := app2.store.addToPool("ADA", []string{addr}); err != nil {
		t.Fatal(err)
	}
	bal = 0
	koiosDepth = 15
	inv2, err := app2.createInvoice(createInvoiceReq{Coin: "ADA", AmountCrypto: 5})
	if err != nil {
		t.Fatal(err)
	}
	bal = 5000000
	app2.checkInvoice(inv2)
	if got := app2.store.Invoices[inv2.ID].State; got != "confirmed" {
		t.Fatalf("ADA with 15 Koios depth: state %q, want confirmed", got)
	}
}

// Solana baseline must use signature lists only, never a full
// transaction fetch: the full listing took >30s live and orphaned
// invoices at the web client's timeout.
func TestSolanaBaselineUsesSignaturesOnly(t *testing.T) {
	var mu sync.Mutex
	var getTxCalls, sigCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case "getSignaturesForAddress":
			sigCalls++
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":[{"signature":"old-sig-1","confirmationStatus":"finalized","err":null}]}`)
		case "getTokenAccountsByOwner":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"value":[{"pubkey":"tokenacct1"}]}}`)
		case "getTransaction":
			getTxCalls++
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":null}`)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":[]}`)
		}
	}))
	defer srv.Close()
	for _, coin := range []string{"SOL", "USDC"} {
		app := testApp(t)
		app.cfg.SolRPC = srv.URL
		addr := testSOLAddr
		if _, _, err := app.store.addToPool(coin, []string{addr}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		getTxCalls, sigCalls = 0, 0
		mu.Unlock()
		inv, err := app.createInvoice(createInvoiceReq{Coin: coin, AmountCrypto: 1})
		if err != nil {
			t.Fatalf("%s create: %v", coin, err)
		}
		if !inv.BaselineDone {
			t.Fatalf("%s: baseline should succeed", coin)
		}
		mu.Lock()
		tx, sig := getTxCalls, sigCalls
		mu.Unlock()
		if tx != 0 {
			t.Fatalf("%s baseline called getTransaction %d times, want 0 (signatures only)", coin, tx)
		}
		if sig == 0 {
			t.Fatalf("%s baseline called getSignaturesForAddress 0 times", coin)
		}
	}
}

// Expiry and late-payment rules, exactly as production needs them:
// no payment past expiry -> expired; a detected/confirming payment
// never expires on its own; a partial still short 24h past expiry ->
// underpaid; a full late payment (within 7 days) -> paid_late.
func TestExpiryAndLatePaymentRules(t *testing.T) {
	newBTCApp := func() (*App, *mockEsplora, *httptest.Server) {
		esp := &mockEsplora{amount: 0, addr: testBTCAddr}
		srv := httptest.NewServer(http.HandlerFunc(esp.handler))
		app := testApp(t)
		app.cfg.BTCExplorer = srv.URL
		app.cfg.Conf["BTC"] = 2
		if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
			t.Fatal(err)
		}
		return app, esp, srv
	}
	// No payment, past expiry -> expired.
	app, _, srv := newBTCApp()
	defer srv.Close()
	inv, _ := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	app.store.Invoices[inv.ID].ExpiresAt = time.Now().Add(-time.Minute)
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "expired" {
		t.Fatalf("no-payment past expiry: state %q, want expired", got)
	}
	// Confirming payment, past expiry but within 24h -> still confirming, not expired.
	app2, esp2, srv2 := newBTCApp()
	defer srv2.Close()
	inv2, _ := app2.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	esp2.amount = int64(inv2.AmountCrypto*1e8 + 0.5)
	esp2.confirmed = false
	app2.checkInvoice(inv2)
	if got := app2.store.Invoices[inv2.ID].State; got != "detected" {
		t.Fatalf("unconfirmed payment: state %q, want detected", got)
	}
	app2.store.Invoices[inv2.ID].ExpiresAt = time.Now().Add(-time.Minute)
	app2.checkInvoice(inv2)
	if got := app2.store.Invoices[inv2.ID].State; got != "detected" {
		t.Fatalf("detected payment past expiry: state %q, want detected (never expires on its own)", got)
	}
	// Partial, 25h past expiry -> underpaid.
	app3, esp3, srv3 := newBTCApp()
	defer srv3.Close()
	inv3, _ := app3.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	esp3.amount = int64(inv3.AmountCrypto*1e8/2 + 0.5)
	app3.checkInvoice(inv3)
	app3.store.Invoices[inv3.ID].ExpiresAt = time.Now().Add(-25 * time.Hour)
	app3.checkInvoice(inv3)
	if got := app3.store.Invoices[inv3.ID].State; got != "underpaid" {
		t.Fatalf("partial 25h past expiry: state %q, want underpaid", got)
	}
	// Full late payment to an expired invoice -> paid_late.
	app4, esp4, srv4 := newBTCApp()
	defer srv4.Close()
	inv4, _ := app4.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	app4.store.Invoices[inv4.ID].ExpiresAt = time.Now().Add(-time.Minute)
	app4.checkInvoice(inv4)
	if app4.store.Invoices[inv4.ID].State != "expired" {
		t.Fatal("setup: late test invoice should be expired first")
	}
	esp4.amount = int64(inv4.AmountCrypto*1e8 + 0.5)
	esp4.confirmed = true
	app4.checkInvoice(inv4)
	if got := app4.store.Invoices[inv4.ID].State; got != "paid_late" {
		t.Fatalf("full late payment: state %q, want paid_late", got)
	}
}

// Quoted amounts stay reserved through the 7-day late window, and
// only then become reusable: a new invoice on a shared address must
// never steal a late payment.
func TestQuotedAmountReservedThroughLateWindow(t *testing.T) {
	app := testApp(t)
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv1, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	// Expire it, but keep it inside the late window.
	app.store.Invoices[inv1.ID].State = "expired"
	app.store.Invoices[inv1.ID].ExpiresAt = time.Now().Add(-time.Hour)
	if app.amountInUse("BTC", inv1.AmountCrypto) != true {
		t.Fatal("expired invoice inside late window must reserve its amount")
	}
	inv2, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if inv2.AmountCrypto == inv1.AmountCrypto {
		t.Fatal("new invoice reused an amount reserved by a late-window invoice")
	}
	// 8 days past expiry: the window is over, the amount is free again.
	app.store.Invoices[inv1.ID].ExpiresAt = time.Now().Add(-8 * 24 * time.Hour)
	if app.amountInUse("BTC", inv1.AmountCrypto) != false {
		t.Fatal("invoice 8 days past expiry must not reserve its amount")
	}
}

// Every coin must have a positive offset unit (shared-address
// uniqueness depends on it) and a payment URI, except XRP and TRX,
// which have no standard URI and must return "" so the pay page
// falls back to address text instead of a broken QR code.
func TestAllCoinsOffsetAndURI(t *testing.T) {
	coins := []string{"BTC", "LTC", "ETH", "MATIC", "ARB", "OP", "BASE", "AVAX", "DOGE", "BCH", "DASH", "ZEC", "ADA", "XRP", "XLM", "TRX", "SOL", "USDC", "USDT", "XMR"}
	cfg := &Config{USDCMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", USDTMint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"}
	for _, coin := range coins {
		if u := offsetUnit(coin); u <= 0 {
			t.Errorf("%s: offset unit %v, want positive", coin, u)
		}
		uri := paymentURI(cfg, &Invoice{Coin: coin, Address: "testaddr123", AmountCrypto: 1})
		switch coin {
		case "XRP", "TRX":
			if uri != "" {
				t.Errorf("%s: URI %q, want empty (no standard URI)", coin, uri)
			}
		default:
			if uri == "" || !strings.Contains(uri, "testaddr123") {
				t.Errorf("%s: URI %q, want non-empty and containing the address", coin, uri)
			}
		}
	}
}
