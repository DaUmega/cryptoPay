package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	store, err := loadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		DataDir: dir, APIToken: "test-token", WebhookSecret: "test-secret",
		ExpiryMin: 60, PollSec: 3600, PriceBaseURL: "https://api.coinpaprika.com",
		USDCMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		USDTMint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
		Conf:     map[string]int{"BTC": 2, "LTC": 6, "ETH": 12, "MATIC": 64, "ARB": 12, "OP": 12, "BASE": 12, "AVAX": 12, "DOGE": 6, "BCH": 6, "DASH": 6, "ZEC": 12, "ADA": 15, "XRP": 1, "XLM": 1, "TRX": 20, "SOL": 32, "USDC": 32, "USDT": 32, "XMR": 10},
	}
	return &App{cfg: cfg, store: store, xmrIdx: map[string][2]uint64{}}
}

func TestDeriveOpenState(t *testing.T) {
	cases := []struct {
		received, confirmed, amount float64
		maxConf                     int
		want                        string
	}{
		{0, 0, 0.01, 0, "awaiting"},
		{0.01, 0, 0.01, 0, "detected"},
		{0.01, 0, 0.01, 1, "confirming"},
		{0.01, 0.01, 0.01, 2, "confirmed"},
		{0.005, 0.005, 0.01, 5, "confirming"},      // partial, even if confirmed
		{0.009999, 0.009999, 0.01, 5, "confirmed"}, // within 0.1% tolerance
	}
	for _, c := range cases {
		if got := deriveOpenState(c.received, c.confirmed, c.amount, c.maxConf); got != c.want {
			t.Errorf("deriveOpenState(%v,%v,%v,%d) = %q, want %q", c.received, c.confirmed, c.amount, c.maxConf, got, c.want)
		}
	}
}

func TestOffsetUniqueness(t *testing.T) {
	app := testApp(t)
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr, testBTCAddr2}); err != nil {
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
	if inv1.AmountCrypto == inv2.AmountCrypto {
		t.Errorf("same base amount gave the same quoted amount: %v", inv1.AmountCrypto)
	}
	if inv1.Address == inv2.Address {
		// Two addresses were in the pool, so the second invoice
		// takes the second one; sharing is covered by the test below.
		t.Errorf("two invoices with two pool addresses shared one: %s", inv1.Address)
	}
	for _, inv := range []*Invoice{inv1, inv2} {
		off := inv.AmountCrypto - inv.BaseAmount
		sats := int64(off/1e-8 + 0.5)
		if sats < 1 || sats > 99 {
			t.Errorf("offset %v (%d satoshis) outside 1-99 satoshis", off, sats)
		}
	}
}

// One pool address must serve many invoices: the second invoice
// reuses the assigned address, with a different quoted amount, and a
// third invoice still succeeds instead of failing with pool empty.
func TestSharedAddressReuse(t *testing.T) {
	app := testApp(t)
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	var addrs []string
	var amounts []float64
	for i := 0; i < 3; i++ {
		inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01})
		if err != nil {
			t.Fatalf("invoice %d: %v", i, err)
		}
		addrs = append(addrs, inv.Address)
		amounts = append(amounts, inv.AmountCrypto)
	}
	for i, a := range addrs {
		if a != testBTCAddr {
			t.Errorf("invoice %d address = %q, want shared %q", i, a, testBTCAddr)
		}
	}
	if amounts[0] == amounts[1] || amounts[0] == amounts[2] || amounts[1] == amounts[2] {
		t.Errorf("shared-address invoices quoted the same amount: %v", amounts)
	}
	if c := app.store.poolCounts("BTC"); c["assigned"] != 1 || c["available"] != 0 {
		t.Fatalf("pool counts = %v, want assigned 1 available 0", c)
	}
}

func TestPoolRetiredReadd(t *testing.T) {
	app := testApp(t)
	added, skipped, err := app.store.addToPool("BTC", []string{testBTCAddr, testBTCAddr})
	if err != nil || added != 1 || skipped != 1 {
		t.Fatalf("add = %d/%d err %v", added, skipped, err)
	}
	addr, err := app.store.assign("BTC", "inv-1")
	if err != nil || addr != testBTCAddr {
		t.Fatalf("assign = %q err %v", addr, err)
	}
	app.store.retire(testBTCAddr)
	if c := app.store.poolCounts("BTC"); c["retired"] != 1 {
		t.Fatalf("counts after retire: %v", c)
	}
	added, _, err = app.store.addToPool("BTC", []string{testBTCAddr})
	if err != nil || added != 1 {
		t.Fatalf("re-add retired = %d err %v", added, err)
	}
	if c := app.store.poolCounts("BTC"); c["available"] != 1 {
		t.Fatalf("counts after re-add: %v", c)
	}
}

func TestPaymentURI(t *testing.T) {
	cfg := &Config{USDCMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", USDTMint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"}
	cases := []struct {
		coin, addr string
		amount     float64
		wantSub    string // "" means expect an empty URI
	}{
		{"BTC", "bc1qxyz", 0.01, "bitcoin:bc1qxyz?amount=0.01"},
		{"ETH", "0xabc", 1.5, "ethereum:0xabc?value="},
		{"ADA", "addr1xyz", 10, "cardano:addr1xyz?amount=10000000"},
		{"SOL", "SoLaddr", 2, "solana:SoLaddr?amount=2&label=cryptopay"},
		{"USDC", "SoLaddr", 5, "spl-token=EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		{"BCH", "bitcoincash:qqat3tag5c0g0u5w54de2jgkrwvdfyxl2c3nh4u52w", 1, "bitcoincash:qqat3tag5c0g0u5w54de2jgkrwvdfyxl2c3nh4u52w?amount=1"},
		{"DASH", "XAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 2, "dash:XAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA?amount=2"},
		{"XLM", "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 10, "web+stellar:pay?destination=GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&amount=10"},
		{"USDT", "SoLaddr", 5, "spl-token=Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"},
		{"XRP", "rXXX", 20, ""},
		{"TRX", "TXXX", 100, ""},
	}
	for _, c := range cases {
		inv := &Invoice{Coin: c.coin, Address: c.addr, AmountCrypto: c.amount}
		got := paymentURI(cfg, inv)
		if c.wantSub == "" {
			if got != "" {
				t.Errorf("%s: expected empty URI, got %q", c.coin, got)
			}
			continue
		}
		if !contains(got, c.wantSub) {
			t.Errorf("%s: URI %q does not contain %q", c.coin, got, c.wantSub)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestWebhookHMAC(t *testing.T) {
	body := []byte(`{"invoice_id":"abc"}`)
	got := signWebhook("secret", body)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want || len(got) != 64 {
		t.Errorf("signWebhook = %q, want %q", got, want)
	}
	if signWebhook("", body) != "" {
		t.Errorf("empty secret must give an empty signature")
	}
}

func TestTronHex(t *testing.T) {
	hexAddr, err := tronHex("TR12xuMvAUKsUhWHDP8yJPxxp9vUknHSYh")
	if err != nil {
		t.Fatal(err)
	}
	if len(hexAddr) != 42 || hexAddr[:2] != "41" {
		t.Errorf("tron hex = %q, want 42 chars starting with 41", hexAddr)
	}
	if _, err := tronHex("not-an-address"); err == nil {
		t.Errorf("bad address must error")
	}
	if raw, err := base58Decode("1"); err != nil || len(raw) != 1 || raw[0] != 0 {
		t.Errorf("base58Decode(1) = %v err %v, want [0]", raw, err)
	}
}

func TestInvoiceResponseURLs(t *testing.T) {
	inv := &Invoice{ID: "abc123", Coin: "BTC", Address: "bc1qxyz", AmountCrypto: 0.01, State: "awaiting", CreatedAt: time.Now(), ExpiresAt: time.Now()}
	v := invoiceResponse(&Config{PublicURL: "https://pay.example.com", Conf: map[string]int{"BTC": 2}}, inv)
	if v.PayPath != "/pay/abc123" || v.PayURL != "https://pay.example.com/pay/abc123" {
		t.Errorf("pay urls = %q %q", v.PayPath, v.PayURL)
	}
	v = invoiceResponse(&Config{Conf: map[string]int{"BTC": 2}}, inv)
	if v.PayURL != "/pay/abc123" {
		t.Errorf("no public URL must give a relative pay URL, got %q", v.PayURL)
	}
}
