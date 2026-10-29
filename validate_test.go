package main

import (
	"strings"
	"testing"
)

func TestValidateAddress(t *testing.T) {
	valid := map[string][]string{
		"BTC":   {testBTCAddr, testBTCAddr2, "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"},
		"LTC":   {testLTCAddr},
		"ETH":   {testETHAddr},
		"MATIC": {testETHAddr},
		"ARB":   {testETHAddr},
		"OP":    {testETHAddr},
		"BASE":  {testETHAddr},
		"AVAX":  {testETHAddr},
		"DOGE":  {testDOGEAddr},
		"BCH":   {testBCHAddr},
		"DASH":  {testDASHAddr},
		"ZEC":   {testZECAddr},
		"ADA":   {"addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzr3jcu5d8ps7zex2k2xt3uqxgjqnnj83ws8lhrn648jjxtw0dh"},
		"XRP":   {"r9cZA1mLK5R5Am25ArfXFmqgNwjZgnfk59"},
		"XLM":   {testXLMAddr},
		"TRX":   {"TR12xuMvAUKsUhWHDP8yJPxxp9vUknHSYh"},
		"SOL":   {testSOLAddr},
		"USDC":  {testSOLAddr},
		"USDT":  {testSOLAddr},
		"XMR":   {"4" + strings.Repeat("A", 94), "8" + strings.Repeat("A", 94)},
	}
	for coin, addrs := range valid {
		for _, addr := range addrs {
			if err := validateAddress(coin, addr); err != nil {
				t.Errorf("%s %q should be valid: %v", coin, addr, err)
			}
		}
	}
	invalid := map[string][]string{
		"BTC":     {"", "bc1qpooladdr", "bc1qshort", "notanaddress", testETHAddr},
		"LTC":     {"", "ltc1qshort", testBTCAddr},
		"ETH":     {"", "0xabc", "0x" + strings.Repeat("g", 40), testBTCAddr},
		"DOGE":    {"", "Dshort", "X" + strings.Repeat("A", 33), "D" + strings.Repeat("0", 33)},
		"BCH":     {"", "qqat3tag5c0g0u5w54de2jgkrwvdfyxl2c3nh4u52w", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
		"DASH":    {"", "D" + strings.Repeat("A", 33)},
		"ZEC":     {"", "z1" + strings.Repeat("A", 33), "t1short"},
		"ADA":     {"", "addr1short", "stake1xyz"},
		"XRP":     {"", "rshort", testETHAddr},
		"XLM":     {"", "Gshort", "G" + strings.Repeat("8", 55)},
		"TRX":     {"", "not-an-address", testETHAddr},
		"SOL":     {"", "short", "SoLaddr with space"},
		"XMR":     {"", "4" + strings.Repeat("A", 50), "9" + strings.Repeat("A", 94)},
		"BNB":     {testETHAddr}, // removed coin: must not validate
		"UNKNOWN": {testBTCAddr},
	}
	for coin, addrs := range invalid {
		for _, addr := range addrs {
			if err := validateAddress(coin, addr); err == nil {
				t.Errorf("%s %q should be invalid", coin, addr)
			}
		}
	}
	// Every accepted coin must have a price mapping, otherwise
	// amount_usd invoices for that coin fail at runtime, not at
	// pool time. This test fails loudly when a coin is added to
	// acceptedCoins without a priceCoinID entry.
	for _, coin := range acceptedCoins {
		if priceCoinID(coin) == "" {
			t.Errorf("accepted coin %s has no price mapping", coin)
		}
		if offsetUnit(coin) <= 0 {
			t.Errorf("accepted coin %s has no offset unit", coin)
		}
		if paymentURI(&Config{USDCMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", USDTMint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"},
			&Invoice{Coin: coin, Address: "x", AmountCrypto: 1}) == "" && coin != "XRP" && coin != "TRX" {
			t.Errorf("accepted coin %s has no payment URI (XRP/TRX are the only expected exceptions)", coin)
		}
	}
}

func TestValidateHTTPURL(t *testing.T) {
	for _, good := range []string{"", "https://example.com/webhook", "http://127.0.0.1:8080/hook", "https://example.com/thank-you?order=1"} {
		if err := validateHTTPURL(good); err != nil {
			t.Errorf("URL %q should be valid: %v", good, err)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,hi", "file:///etc/passwd", "ftp://example.com/x", "https://", "example.com/no-scheme", "https://exa mple.com", "https://" + strings.Repeat("a", 2100)} {
		if err := validateHTTPURL(bad); err == nil {
			t.Errorf("URL %q should be invalid", bad)
		}
	}
}

func TestValidInvoiceID(t *testing.T) {
	if !validInvoiceID("0123456789abcdef0123456789abcdef") {
		t.Error("32 hex chars should be valid")
	}
	for _, bad := range []string{"", "abc", "0123456789abcdef0123456789ABCDEF", "../etc/passwd", "0123456789abcdef0123456789abcdef0", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if validInvoiceID(bad) {
			t.Errorf("ID %q should be invalid", bad)
		}
	}
}
