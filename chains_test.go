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

// Test addresses: format-valid, not real wallets. BTC/LTC/ETH/XRP/ADA
// are well-known example addresses; the rest are synthetic strings
// that pass validateAddress, which is all a mock test needs.
const (
	testBTCAddr  = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	testBTCAddr2 = "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"
	testLTCAddr  = "LVg2kJoFNg45Nbpy53h7Fe1wKyeXVRhZq"
	testETHAddr  = "0x742d35Cc6634C0532925a3b8D9fa7a3D91D1e9b3"
	testDOGEAddr = "DH5yaieqoZN36fDVciNyRueRGvGLR3mr7L"
	testBCHAddr  = "bitcoincash:qqat3tag5c0g0u5w54de2jgkrwvdfyxl2c3nh4u52w"
	testDASHAddr = "XAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testZECAddr  = "t1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testXLMAddr  = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testSOLAddr  = "11111111111111111111111111111111"
)

// Mock Esplora: /address/<addr>/txs and /blocks/tip/height. The test flips
// the tx from unconfirmed to confirmed to walk the state machine.
type mockEsplora struct {
	mu        sync.Mutex
	confirmed bool
	amount    int64 // satoshis to the pool address
	addr      string
}

func (m *mockEsplora) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/blocks/tip/height") {
		fmt.Fprint(w, "101")
		return
	}
	status := map[string]any{"confirmed": false}
	if m.confirmed {
		status = map[string]any{"confirmed": true, "block_height": 100}
	}
	tx := map[string]any{
		"txid":   "btc-tx-1",
		"status": status,
		"vout": []map[string]any{
			{"scriptpubkey_address": m.addr, "value": m.amount},
			{"scriptpubkey_address": testBTCAddr2, "value": 500},
		},
	}
	_ = json.NewEncoder(w).Encode([]any{tx})
}

func TestBTCWatchEndToEnd(t *testing.T) {
	// Payment appears after creation, else it is baseline history.
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()

	var webhookBody []byte
	var webhookSig string
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookBody, _ = io.ReadAll(r.Body)
		webhookSig = r.Header.Get("X-Cryptopay-Signature")
		w.WriteHeader(200)
	}))
	defer hookSrv.Close()

	app := testApp(t)
	app.cfg.BTCExplorer = espSrv.URL
	app.cfg.Conf["BTC"] = 2 // tip 101, block 100 -> 2 confirmations
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01, WebhookURL: hookSrv.URL, MerchantRef: "order-1"})
	if err != nil {
		t.Fatal(err)
	}
	esp.amount = 1000000 // 0.01 BTC, arriving after creation
	// Unconfirmed payment: detected.
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "detected" {
		t.Fatalf("after unconfirmed tx: state = %q, want detected", got)
	}
	// Confirm it (block 100, tip 101 = 2 confirmations): confirmed + webhook.
	esp.mu.Lock()
	esp.confirmed = true
	esp.mu.Unlock()
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "confirmed" {
		t.Fatalf("after confirmed tx: state = %q, want confirmed", got.State)
	}
	if got.Received != 0.01 || got.ConfirmedReceived != 0.01 {
		t.Fatalf("received = %v confirmed = %v, want 0.01/0.01", got.Received, got.ConfirmedReceived)
	}
	// Webhook fires async; wait briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if webhookBody != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if webhookBody == nil {
		t.Fatal("webhook was not delivered")
	}
	if webhookSig == "" {
		t.Fatal("webhook has no HMAC signature")
	}
	var payload map[string]any
	if err := json.Unmarshal(webhookBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["invoice_id"] != inv.ID || payload["state"] != "confirmed" {
		t.Fatalf("webhook payload = %v", payload)
	}
}

func TestPriceConversion(t *testing.T) {
	priceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"price_usd": 50000}`)
	}))
	defer priceSrv.Close()
	app := testApp(t)
	app.cfg.PriceAPIURL = priceSrv.URL
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr2}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	if inv.BaseAmount != 0.002 {
		t.Fatalf("base = %v, want 0.002 (100 / 50000)", inv.BaseAmount)
	}
	off := inv.AmountCrypto - inv.BaseAmount
	// Compare in whole satoshis, not raw floats: 99 * 1e-8 is one
	// float ulp above the literal 99e-8, which used to fail this
	// test about 1% of the time for no production reason.
	sats := int64(off/1e-8 + 0.5)
	if sats < 1 || sats > 99 {
		t.Fatalf("offset = %v (%d satoshis), want 1-99 satoshis", off, sats)
	}
}

func TestPoolEmptyError(t *testing.T) {
	app := testApp(t)
	_, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("empty pool must error about the pool, got %v", err)
	}
}

func TestExpiryAndLatePayment(t *testing.T) {
	// No payment at creation, else it is baseline history.
	esp := &mockEsplora{amount: 0, confirmed: true, addr: testBTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = espSrv.URL
	app.cfg.Conf["BTC"] = 2
	app.cfg.ExpiryMin = 0 // expires immediately
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	// No payment yet, past expiry: expired, address retired.
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "expired" {
		t.Fatalf("state = %q, want expired", got.State)
	}
	if c := app.store.poolCounts("BTC"); c["retired"] != 1 {
		t.Fatalf("pool after expiry: %v", c)
	}
	// A payment arrives late to the retired address: it must not be ignored.
	esp.amount = 1000000 // 0.01 BTC, arriving after expiry
	app.checkInvoice(inv)
	got = app.store.Invoices[inv.ID]
	if got.State != "paid_late" {
		t.Fatalf("state after late payment = %q, want paid_late", got.State)
	}
	if got.Received != 0.01 {
		t.Fatalf("received after late payment = %v, want 0.01", got.Received)
	}
}

func TestHTTPAPI(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	// Health, no auth.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz = %v err %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	// Create without a token: 401.
	resp, err = http.Post(srv.URL+"/invoices", "application/json", strings.NewReader(`{"coin":"BTC","amount_crypto":0.01}`))
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated create = %v err %v, want 401", resp.StatusCode, err)
	}
	resp.Body.Close()
	// Create with a token, then fetch the invoice and the pay page.
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/invoices", strings.NewReader(`{"coin":"BTC","amount_crypto":0.01}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("create = %v err %v, want 201", resp.StatusCode, err)
	}
	var created invoiceView
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.Address != testBTCAddr || created.PayPath != "/pay/"+created.ID {
		t.Fatalf("created = %+v", created)
	}
	resp, err = http.Get(srv.URL + "/invoices/" + created.ID)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("get invoice = %v err %v, want 200", resp.StatusCode, err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/pay/" + created.ID)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("pay page = %v err %v, want 200", resp.StatusCode, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), testBTCAddr) {
		t.Fatalf("pay page does not show the address")
	}
	if strings.Contains(string(body), "qrserver") {
		t.Fatalf("pay page must not load a third-party QR image")
	}
}

// New-coin tests: LTC reuses the BTC Esplora shape, EVM chains reuse the
// ETH txlist shape with no key, Blockbook covers DOGE/BCH/DASH/ZEC, and
// XMR uses a mock view-only wallet RPC.

func TestLTCReusesEsplora(t *testing.T) {
	// Payment must appear AFTER invoice creation: a payment already
	// present at creation is baseline history, not this invoice's,
	// and must be ignored (see TestOldTxDoesNotConfirm).
	esp := &mockEsplora{amount: 0, addr: testLTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()
	app := testApp(t)
	app.cfg.LTCExplorer = espSrv.URL
	app.cfg.Conf["LTC"] = 2
	if _, _, err := app.store.addToPool("LTC", []string{testLTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "LTC", AmountCrypto: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	esp.amount = 2000000 // 0.02 LTC, arriving after creation
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "detected" {
		t.Fatalf("LTC after unconfirmed tx: state = %q, want detected", got)
	}
	esp.mu.Lock()
	esp.confirmed = true
	esp.mu.Unlock()
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "confirmed" {
		t.Fatalf("LTC after confirmed tx: state = %q, want confirmed", got)
	}
}

func TestEVMChainNoKey(t *testing.T) {
	var gotKey string
	var sawModule bool
	pay := false // payment appears only after creation, else it is baseline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("apikey")
		sawModule = r.URL.Query().Get("module") == "account" && r.URL.Query().Get("action") == "txlist"
		if !pay {
			fmt.Fprint(w, `{"status":"1","result":[]}`)
			return
		}
		fmt.Fprint(w, `{"status":"1","result":[{"hash":"0xmaticstx","to":"`+testETHAddr+`","value":"1000000000000000000","confirmations":"64","isError":"0"}]}`)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.MATICAPI = srv.URL
	if _, _, err := app.store.addToPool("MATIC", []string{testETHAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "MATIC", AmountCrypto: 1})
	if err != nil {
		t.Fatal(err)
	}
	pay = true
	app.checkInvoice(inv)
	if gotKey != "" {
		t.Fatalf("EVM request must not send an apikey, got %q", gotKey)
	}
	if !sawModule {
		t.Fatalf("EVM request must be a txlist query")
	}
	if got := app.store.Invoices[inv.ID].State; got != "confirmed" {
		t.Fatalf("MATIC after 64-conf tx: state = %q, want confirmed", got)
	}
	if got := app.store.Invoices[inv.ID].ConfirmedReceived; got != 1 {
		t.Fatalf("MATIC confirmed = %v, want 1", got)
	}
}

func TestBlockbookCoins(t *testing.T) {
	cases := []struct {
		coin, addr string
		amount     float64
	}{
		{"DOGE", testDOGEAddr, 2.5},
		{"BCH", testBCHAddr, 1.5},
		{"DASH", testDASHAddr, 3.0},
		{"ZEC", testZECAddr, 0.75},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"transactions":[{"txid":"bb-tx-1","blockHeight":100,"confirmations":6,"vout":[{"addresses":["%s"],"value":"%d"}]}]}`, c.addr, int64(c.amount*1e8))
		}))
		app := testApp(t)
		switch c.coin {
		case "DOGE":
			app.cfg.DOGEBlockbook = srv.URL
		case "BCH":
			app.cfg.BCHBlockbook = srv.URL
		case "DASH":
			app.cfg.DASHBlockbook = srv.URL
		case "ZEC":
			app.cfg.ZECBlockbook = srv.URL
		}
		pays, err := app.listPayments(c.coin, c.addr, nil)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.coin, err)
		}
		if len(pays) != 1 || pays[0].Amount != c.amount || pays[0].Confirmations != 6 {
			t.Fatalf("%s payments = %+v, want one %.2f at 6 conf", c.coin, pays, c.amount)
		}
	}
	// No Blockbook URL configured: error, never silent empty.
	app := testApp(t)
	if _, err := app.listPayments("DOGE", testDOGEAddr, nil); err == nil {
		t.Fatal("DOGE with no Blockbook URL should error, not silently return nothing")
	}
}

func TestXMRWalletRPC(t *testing.T) {
	xmrAddr := "4" + strings.Repeat("A", 94)
	pay := false // transfers appear only after creation, else baseline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "xmr-user" || pass != "xmr-pass" {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "get_address_index":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"0","result":{"index":{"major":0,"minor":3}}}`)
		case "get_transfers":
			if !pay {
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":"0","result":{"in":[],"pool":[]}}`)
				return
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"0","result":{"in":[{"txid":"xmr-tx-1","amount":2500000000000,"confirmations":12,"address":"`+xmrAddr+`"}],"pool":[{"txid":"xmr-tx-2","amount":100000000000,"address":"`+xmrAddr+`"}]}}`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.XMRWalletRPC = srv.URL
	app.cfg.XMRWalletUser = "xmr-user"
	app.cfg.XMRWalletPass = "xmr-pass"
	if _, _, err := app.store.addToPool("XMR", []string{xmrAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "XMR", AmountCrypto: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	pay = true
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "confirmed" {
		t.Fatalf("XMR after 12-conf tx: state = %q, want confirmed", got.State)
	}
	// 2.5 confirmed + 0.1 in mempool = 2.6 received, 2.5 confirmed.
	if got.Received != 2.6 || got.ConfirmedReceived != 2.5 {
		t.Fatalf("XMR received = %v confirmed = %v, want 2.6/2.5", got.Received, got.ConfirmedReceived)
	}
	// The index lookup is cached: a second poll must not need it again.
	idx, err := app.xmrIndex(xmrAddr)
	if err != nil || idx != [2]uint64{0, 3} {
		t.Fatalf("XMR cached index = %v err %v, want [0 3]", idx, err)
	}
	// The URI carries the quoted amount, which includes the 1-99 piconero
	// offset, so it starts with 2.5 but is not exactly 2.5.
	if uri := paymentURI(app.cfg, inv); !strings.HasPrefix(uri, "monero:"+xmrAddr+"?amount=2.5") {
		t.Fatalf("XMR URI = %q", uri)
	}
}

func TestStellarPayments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"_embedded":{"records":[
			{"type":"payment","asset_type":"native","to":"%s","amount":"10.5","transaction_hash":"xlm-tx-1"},
			{"type":"payment","asset_type":"credit_alphanum4","to":"%s","amount":"99","transaction_hash":"xlm-token-tx"},
			{"type":"payment","asset_type":"native","to":"GOTHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","amount":"5","transaction_hash":"xlm-other-tx"}
		]}}`, testXLMAddr, testXLMAddr)
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.XLMHorizon = srv.URL
	pays, err := app.listPayments("XLM", testXLMAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pays) != 1 || pays[0].Amount != 10.5 || pays[0].TxID != "xlm-tx-1" {
		t.Fatalf("XLM payments = %+v, want only the native payment to this address", pays)
	}
	if pays[0].Confirmations != 999 {
		t.Fatalf("XLM confirmations = %d, want 999 (final)", pays[0].Confirmations)
	}
}

func TestSolanaTokenPayments(t *testing.T) {
	// Mock Solana RPC: token account lookup, then signatures, then a
	// transaction whose post token balance exceeds its pre balance.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "getTokenAccountsByOwner":
			fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"value":[{"pubkey":"TokenAcct11111111111111111111111111111"}]}}`)
		case "getSignaturesForAddress":
			fmt.Fprint(w, `{"jsonrpc":"2.0","result":[{"signature":"solsig1","confirmationStatus":"finalized","err":null}]}`)
		case "getTransaction":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","result":{"meta":{"preTokenBalances":[],"postTokenBalances":[{"owner":"%s","mint":"%s","uiTokenAmount":{"uiAmount":25}}]}}}`,
				testSOLAddr, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	app := testApp(t)
	app.cfg.SolRPC = srv.URL
	pays, err := app.listPayments("USDC", testSOLAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pays) != 1 || pays[0].Amount != 25 || pays[0].Confirmations != 32 {
		t.Fatalf("USDC payments = %+v, want one 25 at finalized (32)", pays)
	}
	// No token account: no payments, not an error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"value":[]}}`)
	}))
	defer srv2.Close()
	app.cfg.SolRPC = srv2.URL
	pays, err = app.listPayments("USDT", testSOLAddr, nil)
	if err != nil || len(pays) != 0 {
		t.Fatalf("USDT with no token account: pays = %+v err %v, want empty", pays, err)
	}
}
