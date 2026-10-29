package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Gaps found by auditing the Oct 2026 bug list against the existing
// tests. Each test here pins one rule the old tests only implied:
// assign order, claimsPayment against an already-claimed or
// late-window full match, baseline partial/overpayment acceptance,
// full-but-confirming never expiring, late-window polling through
// checkAll (not just checkInvoice), all coin decimals, XMR edges,
// default mint constants, and explicit ADA zero-confirmation.

// assign() order: oldest available first; with none available, the
// oldest assigned; with none assigned, the oldest retired.
func TestAssignPreferenceOrder(t *testing.T) {
	app := testApp(t)
	addrs := []string{testBTCAddr, testBTCAddr2, "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"}
	if _, _, err := app.store.addToPool("BTC", addrs); err != nil {
		t.Fatal(err)
	}
	if got, err := app.store.assign("BTC", "inv-1"); err != nil || got != addrs[0] {
		t.Fatalf("first assign = %q err %v, want oldest available %q", got, err, addrs[0])
	}
	if got, err := app.store.assign("BTC", "inv-2"); err != nil || got != addrs[1] {
		t.Fatalf("second assign = %q err %v, want next available %q", got, err, addrs[1])
	}
	if got, err := app.store.assign("BTC", "inv-3"); err != nil || got != addrs[2] {
		t.Fatalf("third assign = %q err %v, want last available %q", got, err, addrs[2])
	}
	// No available left: reuse the oldest assigned, not a newer one.
	if got, err := app.store.assign("BTC", "inv-4"); err != nil || got != addrs[0] {
		t.Fatalf("no-available assign = %q err %v, want oldest assigned %q", got, err, addrs[0])
	}
	// Retire all three (no open invoices, so retire is allowed),
	// then the oldest retired is reused first.
	for _, a := range addrs {
		app.store.retire(a)
	}
	if got, err := app.store.assign("BTC", "inv-5"); err != nil || got != addrs[0] {
		t.Fatalf("no-assigned assign = %q err %v, want oldest retired %q", got, err, addrs[0])
	}
}

// Quoted amounts are reserved for every open state, not just
// awaiting, and for expired/underpaid inside the 7-day window only.
func TestAmountReservedForOpenStates(t *testing.T) {
	app := testApp(t)
	mk := func(id, state string, expires time.Time) {
		app.store.Invoices[id] = &Invoice{ID: id, Coin: "BTC", Address: testBTCAddr,
			AmountCrypto: 0.01, State: state, ExpiresAt: expires}
	}
	now := time.Now()
	for i, state := range []string{"awaiting", "detected", "confirming"} {
		mk(fmt.Sprintf("open-%d", i), state, now.Add(time.Hour))
	}
	mk("late-exp", "expired", now.Add(-time.Hour))
	mk("late-und", "underpaid", now.Add(-time.Hour))
	mk("old-exp", "expired", now.Add(-8*24*time.Hour))
	mk("paid", "paid_late", now.Add(-time.Hour))
	mk("conf", "confirmed", now.Add(time.Hour))
	if !app.amountInUse("BTC", 0.01) {
		t.Fatal("open/late invoices must reserve their quoted amount")
	}
	// Remove the reserving invoices: only old-expired and terminal
	// ones left, so the amount must be free again.
	for _, id := range []string{"open-0", "open-1", "open-2", "late-exp", "late-und"} {
		delete(app.store.Invoices, id)
	}
	if app.amountInUse("BTC", 0.01) {
		t.Fatal("expired past the late window and terminal invoices must not reserve an amount")
	}
}

// claimsPayment, direct cases the end-to-end tests do not isolate:
// a txid already claimed by another invoice, a tx that full-matches
// another open or late-window invoice (must not be taken as a
// partial for this one), and, with a baseline, partial/overpayment
// acceptance that the no-baseline tests only prove by rejection.
func TestClaimsPaymentAlreadyClaimedAndLateFullMatch(t *testing.T) {
	app := testApp(t)
	cur := &Invoice{ID: "cur", Coin: "BTC", Address: testBTCAddr, AmountCrypto: 20, BaselineDone: true}
	other := &Invoice{ID: "other", Coin: "BTC", Address: testBTCAddr, AmountCrypto: 10,
		State: "awaiting", Payments: []Payment{{TxID: "claimed-tx", Amount: 10}}}
	app.store.Invoices[cur.ID] = cur
	app.store.Invoices[other.ID] = other
	if app.claimsPayment(cur, Payment{TxID: "claimed-tx", Amount: 20}, 0) {
		t.Fatal("txid already in another invoice's Payments must never be claimed")
	}
	// Full match to the other invoice, offered as a partial to cur.
	if app.claimsPayment(cur, Payment{TxID: "new-tx", Amount: 10}, 0) {
		t.Fatal("tx full-matching another open invoice must not be claimed as a partial")
	}
	// Same rule for a late-window invoice: it still owns its amount.
	other.State = "expired"
	other.ExpiresAt = time.Now().Add(-time.Hour)
	other.Payments = nil
	if app.claimsPayment(cur, Payment{TxID: "new-tx", Amount: 10}, 0) {
		t.Fatal("tx full-matching another late-window invoice must not be claimed as a partial")
	}
	// Baseline acceptance: partial, top-up, and plain overpayment.
	if !app.claimsPayment(cur, Payment{TxID: "part", Amount: 5}, 0) {
		t.Fatal("baseline partial must be claimed")
	}
	if !app.claimsPayment(cur, Payment{TxID: "topup", Amount: 6}, 14) {
		t.Fatal("baseline top-up to the full amount must be claimed")
	}
	if !app.claimsPayment(cur, Payment{TxID: "over", Amount: 25}, 0) {
		t.Fatal("baseline overpayment must be claimed")
	}
}

// A full payment that is still confirming must never expire, even
// 25h past expiry: the money is in full, only confirmations are
// outstanding, so neither the no-payment nor the partial rule fires.
func TestFullConfirmingNeverExpires(t *testing.T) {
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	srv := httptest.NewServer(http.HandlerFunc(esp.handler))
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
	got := app.store.Invoices[inv.ID]
	got.State = "confirming"
	got.Received = got.AmountCrypto
	got.Payments = []Payment{{TxID: "btc-tx-1", Amount: got.AmountCrypto, Confirmations: 1}}
	got.ExpiresAt = time.Now().Add(-25 * time.Hour)
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "confirming" {
		t.Fatalf("full confirming 25h past expiry: state %q, want confirming", got)
	}
}

// Late-window polling is a checkAll rule, not a checkInvoice rule:
// an underpaid invoice paid in full inside the window becomes
// paid_late, and an expired invoice 8 days out is not polled at
// all, so its late payment must stay unclaimed.
func TestLateWindowPollingViaCheckAll(t *testing.T) {
	// Underpaid -> paid_late inside the window.
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	srv := httptest.NewServer(http.HandlerFunc(esp.handler))
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
	esp.amount = int64(inv.AmountCrypto*1e8/2 + 0.5)
	app.checkInvoice(inv)
	app.store.Invoices[inv.ID].ExpiresAt = time.Now().Add(-25 * time.Hour)
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "underpaid" {
		t.Fatalf("setup: state %q, want underpaid", got)
	}
	esp.amount = int64(inv.AmountCrypto*1e8 + 0.5)
	esp.confirmed = true
	app.checkAll()
	if got := app.store.Invoices[inv.ID].State; got != "paid_late" {
		t.Fatalf("underpaid paid in full: state %q, want paid_late", got)
	}

	// Expired 8 days out: checkAll must not poll it.
	app2 := testApp(t)
	app2.cfg.BTCExplorer = srv.URL
	app2.cfg.Conf["BTC"] = 2
	if _, _, err := app2.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv2, err := app2.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	app2.store.Invoices[inv2.ID].State = "expired"
	app2.store.Invoices[inv2.ID].ExpiresAt = time.Now().Add(-8 * 24 * time.Hour)
	app2.checkAll()
	if got := app2.store.Invoices[inv2.ID]; got.State != "expired" || got.Received != 0 {
		t.Fatalf("out-of-window expired invoice polled: state %q received %v", got.State, got.Received)
	}
}

// Every coin's decimals, not just the ones in the original XMR
// report, plus the stored invoice value and webhook payload: a
// noisy amount_crypto must never survive createInvoice, JSON, or
// a webhook body.
func TestRoundingAllCoinDecimals(t *testing.T) {
	wantDec := map[string]int{
		"BTC": 8, "LTC": 8, "DOGE": 8, "BCH": 8, "DASH": 8, "ZEC": 8,
		"ETH": 9, "MATIC": 9, "ARB": 9, "OP": 9, "BASE": 9, "AVAX": 9, "SOL": 9,
		"ADA": 6, "XRP": 6, "TRX": 6, "USDC": 6, "USDT": 6,
		"XLM": 7, "XMR": 12,
	}
	for coin, dec := range wantDec {
		if got := coinDecimals(coin); got != dec {
			t.Errorf("%s decimals = %d, want %d", coin, got, dec)
		}
	}
	// Noisy input per family formats clean, and clean stays short.
	for _, c := range []struct {
		coin string
		in   float64
		want string
	}{
		{"BTC", 0.010000000000000002, "0.01"},
		{"LTC", 0.010000000000000002, "0.01"},
		{"ETH", 0.12345678900000001, "0.123456789"},
		{"SOL", 0.12345678900000001, "0.123456789"},
		{"ADA", 10.000000000000002, "10"},
		{"XRP", 20.000000000000004, "20"},
		{"XLM", 0.12345679999999999, "0.1234568"},
		{"XMR", 0.0016980000280000002, "0.001698000028"},
		{"USDC", 5.000000000000001, "5"},
	} {
		if got := fmtCoinAmount(c.coin, c.in); got != c.want {
			t.Errorf("%s fmtCoinAmount(%v) = %q, want %q", c.coin, c.in, got, c.want)
		}
	}
	// Stored value: a noisy BTC amount_crypto, once created, is an
	// integer number of satoshis, and its JSON carries no noise.
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	srv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer srv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = srv.URL
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.010000000000000002})
	if err != nil {
		t.Fatal(err)
	}
	sats := inv.AmountCrypto * 1e8
	if math.Abs(sats-math.Round(sats)) > 1e-6 {
		t.Fatalf("stored BTC amount %v is not an integer number of satoshis", inv.AmountCrypto)
	}
	b, err := json.Marshal(invoiceResponse(app.cfg, inv))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "00000002") {
		t.Fatalf("invoice JSON carries float noise: %s", b)
	}
	// Webhook payload uses the same rounded form.
	wb, err := json.Marshal(webhookPayload{Coin: "XMR", AmountCrypto: 0.0016980000280000002, State: "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wb), `"amount_crypto":0.001698000028`) {
		t.Fatalf("webhook JSON = %s, want amount_crypto 0.001698000028", wb)
	}
	// Direct-amount payment URIs for the remaining coins.
	cfg := &Config{USDCMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", USDTMint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"}
	for coin, want := range map[string]string{
		"LTC": "litecoin:addr?amount=0.01", "DOGE": "dogecoin:addr?amount=0.01",
		"DASH": "dash:addr?amount=0.01", "ZEC": "zcash:addr?amount=0.01",
		"USDC": "spl-token=EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		"USDT": "spl-token=Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
	} {
		uri := paymentURI(cfg, &Invoice{Coin: coin, Address: "addr", AmountCrypto: 0.010000000000000002})
		if !strings.Contains(uri, want) {
			t.Errorf("%s URI %q, want %q", coin, uri, want)
		}
	}
}

// XMR edges beyond 4/8 valid and 9 invalid: wrong lengths both
// ways, and non-base58 characters inside a 95-char address.
func TestXMRValidatorEdges(t *testing.T) {
	for _, addr := range []string{
		"4" + strings.Repeat("z", 94), "8" + strings.Repeat("z", 94),
		"4" + strings.Repeat("1", 94),
	} {
		if err := validateAddress("XMR", addr); err != nil {
			t.Errorf("XMR %q should be valid: %v", addr[:1]+"...", err)
		}
	}
	for _, addr := range []string{
		"4" + strings.Repeat("A", 93), "4" + strings.Repeat("A", 95),
		"8" + strings.Repeat("A", 93), "8" + strings.Repeat("A", 95),
		"4" + strings.Repeat("A", 93) + "0", "8" + strings.Repeat("A", 93) + "O",
		"4" + strings.Repeat("A", 93) + "l", "8" + strings.Repeat("A", 93) + "I",
	} {
		if err := validateAddress("XMR", addr); err == nil {
			t.Errorf("XMR %q should be invalid", addr)
		}
	}
}

// Default mint constants and L2 price mapping, from loadConfig
// defaults, not from a test fixture that hardcodes the same values.
func TestDefaultMintConstants(t *testing.T) {
	t.Setenv("API_TOKEN", "test-token-for-mint-check")
	t.Setenv("USDC_MINT", "")
	t.Setenv("USDT_MINT", "")
	cfg := loadConfig()
	if cfg.USDCMint != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" {
		t.Errorf("default USDC mint = %q, want the Solana SPL mint", cfg.USDCMint)
	}
	if cfg.USDTMint != "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB" {
		t.Errorf("default USDT mint = %q, want the Solana SPL mint", cfg.USDTMint)
	}
	for _, coin := range []string{"ARB", "OP", "BASE"} {
		if priceCoinID(coin) != "eth-ethereum" || priceSymbol(coin) != "ETH" {
			t.Errorf("%s priced as %q/%q, want eth-ethereum/ETH", coin, priceCoinID(coin), priceSymbol(coin))
		}
	}
}

// Offsets in integer units, not floats: XMR piconero and ETH gwei,
// where a float comparison would hide a one-unit error.
func TestOffsetInIntegerUnits(t *testing.T) {
	app := testApp(t)
	xmrAddr := "4" + strings.Repeat("A", 94)
	if _, _, err := app.store.addToPool("XMR", []string{xmrAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "XMR", AmountCrypto: 1})
	if err != nil {
		t.Fatal(err)
	}
	off := inv.AmountCrypto - inv.BaseAmount
	pico := int64(off/1e-12 + 0.5)
	if pico < 1 || pico > 99 {
		t.Errorf("XMR offset %v (%d piconero) outside 1-99", off, pico)
	}
	if math.Abs(float64(pico)*1e-12-off) > 1e-15 {
		t.Errorf("XMR offset %v is not an integer number of piconero", off)
	}
}

// ADA with Koios silent: confirmations must be explicitly 0, not
// just "not confirmed" by state. Balance alone, even covering the
// amount, must not invent confirmations.
func TestADAConfirmationsExplicitlyZero(t *testing.T) {
	var bal int64
	adaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".json") {
			fmt.Fprintf(w, `{"data":{"balance":%d},"code":200}`, bal)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer adaSrv.Close()
	addr := "addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"
	app := testApp(t)
	app.cfg.AdaStatURL = adaSrv.URL
	app.cfg.KoiosURL = adaSrv.URL
	app.cfg.Conf["ADA"] = 15
	if _, _, err := app.store.addToPool("ADA", []string{addr}); err != nil {
		t.Fatal(err)
	}
	bal = 0
	inv, err := app.createInvoice(createInvoiceReq{Coin: "ADA", AmountCrypto: 5})
	if err != nil {
		t.Fatal(err)
	}
	bal = 5000000
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "detected" {
		t.Fatalf("ADA Koios silent: state %q, want detected", got.State)
	}
	if len(got.Payments) == 0 || got.Payments[0].Confirmations != 0 {
		t.Fatalf("ADA Koios silent: payments %+v, want one payment at 0 confirmations", got.Payments)
	}
	if got.ConfirmedReceived != 0 {
		t.Fatalf("ADA Koios silent: confirmed received %v, want 0", got.ConfirmedReceived)
	}
}
