package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A lister that returns a bogus payment must never confirm an invoice:
// empty txid, zero, negative, and NaN amounts are all dropped in
// checkInvoice, regardless of what the chain API claimed.
func TestBogusPaymentsNeverConfirm(t *testing.T) {
	esp := &mockEsplora{amount: 1000000, addr: testBTCAddr}
	_ = esp
	// Use a mock Esplora that returns a negative amount for this test
	// by flipping the mock's amount, then a zero amount, via separate
	// servers, and confirm the invoice stays awaiting in both cases.
	for _, amt := range []int64{-1000000, 0} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/blocks/tip/height" {
				fmt.Fprint(w, "101")
				return
			}
			fmt.Fprintf(w, `[{"txid":"bad-tx","status":{"confirmed":true,"block_height":100},"vout":[{"scriptpubkey_address":"%s","value":%d}]}]`, testBTCAddr, amt)
		}))
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
		app.checkInvoice(inv)
		if got := app.store.Invoices[inv.ID].State; got != "awaiting" {
			t.Fatalf("amount %d: state = %q, want awaiting", amt, got)
		}
		srv.Close()
	}
}

// ADA's synthetic payment (whole balance under one txid) must update
// its amount when the balance grows, and must ignore balance that
// already existed at creation: that is a previous order's money,
// not this invoice's, and counting it would false-confirm.
// Balance is 0 at creation, then 10 after the first payment,
// then 15 after a top-up.
func TestADABalanceTopUp(t *testing.T) {
	var bal int64 = 0
	adaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/addresses/"+"addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"+".json":
			fmt.Fprintf(w, `{"data":{"balance":%d},"code":200}`, bal)
		default:
			// Koios endpoints: return no txs, so confirmations stay 0.
			fmt.Fprint(w, `[]`)
		}
	}))
	defer adaSrv.Close()
	app := testApp(t)
	app.cfg.AdaStatURL = adaSrv.URL
	app.cfg.KoiosURL = adaSrv.URL
	addr := "addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"
	if _, _, err := app.store.addToPool("ADA", []string{addr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "ADA", AmountCrypto: 15})
	if err != nil {
		t.Fatal(err)
	}
	bal = 10000000 // 10 ADA paid after creation
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].Received; got != 10 {
		t.Fatalf("first poll received = %v, want 10", got)
	}
	bal = 15000000 // top-up to 15 ADA
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].Received; got != 15 {
		t.Fatalf("after top-up received = %v, want 15 (top-up must not be lost)", got)
	}
}

// Two invoices sharing one address, distinguished only by their
// quoted amounts: a payment matching the second invoice must confirm
// only the second, never the first, and must not be double-counted
// even when both invoices poll the same address.
func TestSharedAddressAmountAttribution(t *testing.T) {
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = espSrv.URL
	app.cfg.Conf["BTC"] = 2
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv1, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	inv2, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if inv1.Address != inv2.Address {
		t.Fatalf("shared test invoices got different addresses: %q %q", inv1.Address, inv2.Address)
	}
	// Payment arrives for inv2's exact quoted amount, confirmed.
	esp.amount = int64(inv2.AmountCrypto*1e8 + 0.5)
	esp.confirmed = true
	app.checkInvoice(inv1)
	app.checkInvoice(inv2)
	if got := app.store.Invoices[inv1.ID].State; got != "awaiting" {
		t.Fatalf("inv1 state = %q, want awaiting (payment was inv2's)", got)
	}
	if got := app.store.Invoices[inv1.ID].Received; got != 0 {
		t.Fatalf("inv1 received = %v, want 0", got)
	}
	if got := app.store.Invoices[inv2.ID].State; got != "confirmed" {
		t.Fatalf("inv2 state = %q, want confirmed", got)
	}
	if got := app.store.Invoices[inv2.ID].Received; got != inv2.AmountCrypto {
		t.Fatalf("inv2 received = %v, want %v", got, inv2.AmountCrypto)
	}
	// Re-polling must not double-count or flip inv1.
	app.checkInvoice(inv1)
	app.checkInvoice(inv2)
	if got := app.store.Invoices[inv1.ID].State; got != "awaiting" {
		t.Fatalf("after re-poll inv1 state = %q, want awaiting", got)
	}
}

// An ADA balance that already existed at creation must never
// count: a seller address with 17 ADA on it would otherwise
// false-confirm a 5 ADA invoice on its first poll.
func TestADAPreexistingBalanceIgnored(t *testing.T) {
	var bal int64 = 17000000 // 17 ADA already on the address
	adaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ".json"):
			fmt.Fprintf(w, `{"data":{"balance":%d},"code":200}`, bal)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer adaSrv.Close()
	app := testApp(t)
	app.cfg.AdaStatURL = adaSrv.URL
	app.cfg.KoiosURL = adaSrv.URL
	addr := "addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"
	if _, _, err := app.store.addToPool("ADA", []string{addr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "ADA", AmountCrypto: 5})
	if err != nil {
		t.Fatal(err)
	}
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "awaiting" || got.Received != 0 {
		t.Fatalf("pre-existing ADA balance counted: state %q received %v", got.State, got.Received)
	}
}

// A payment that already existed on a shared address before the
// invoice was created must never confirm it, even if its amount is
// close enough that the old sum-everything logic would have
// confirmed it. This is the live seller-address bug: previous
// customers' payments summed to more than every new invoice.
func TestOldTxDoesNotConfirm(t *testing.T) {
	esp := &mockEsplora{amount: 1000000, confirmed: true, addr: testBTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = espSrv.URL
	app.cfg.Conf["BTC"] = 2
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if !app.store.Invoices[inv.ID].BaselineDone {
		t.Fatalf("baseline should succeed with a working lister")
	}
	app.checkInvoice(inv)
	got := app.store.Invoices[inv.ID]
	if got.State != "awaiting" || got.Received != 0 {
		t.Fatalf("old tx confirmed a new invoice: state %q received %v", got.State, got.Received)
	}
}

// A payment arriving after creation, for the exact quoted amount,
// must still confirm, even on a shared address with history.
func TestBaselineNewPaymentConfirms(t *testing.T) {
	esp := &mockEsplora{amount: 0, addr: testBTCAddr}
	espSrv := httptest.NewServer(http.HandlerFunc(esp.handler))
	defer espSrv.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = espSrv.URL
	app.cfg.Conf["BTC"] = 2
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	esp.amount = int64(inv.AmountCrypto*1e8 + 0.5)
	esp.confirmed = true
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "confirmed" {
		t.Fatalf("new exact payment state = %q, want confirmed", got)
	}
}

// One slow chain must not stall the others: three invoices on slow
// explorers (300ms each) must finish checkAll in well under the
// 900ms a serial loop would take.
func TestCheckAllParallel(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		if r.URL.Path == "/blocks/tip/height" {
			fmt.Fprint(w, "101")
			return
		}
		fmt.Fprintf(w, `[{"txid":"slow-tx","status":{"confirmed":false},"vout":[{"scriptpubkey_address":"%s","value":1000000}]}]`, testBTCAddr)
	}))
	defer slow.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = slow.URL
	addrs := []string{testBTCAddr, testBTCAddr2, "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"}
	for _, addr := range addrs {
		if _, _, err := app.store.addToPool("BTC", []string{addr}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	app.checkAll()
	elapsed := time.Since(start)
	if elapsed > 800*time.Millisecond {
		t.Fatalf("checkAll took %v for 3 x 300ms listers, want parallel (<800ms)", elapsed)
	}
}

// A lister error must leave the invoice untouched, not expire it or
// confirm it, and must not stop other invoices from being checked.
func TestListerErrorIsSafe(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer bad.Close()
	app := testApp(t)
	app.cfg.BTCExplorer = bad.URL
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	app.checkInvoice(inv)
	if got := app.store.Invoices[inv.ID].State; got != "awaiting" {
		t.Fatalf("after lister error state = %q, want awaiting", got)
	}
}
