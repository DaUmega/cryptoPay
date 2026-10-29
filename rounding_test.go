package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression for the live XMR report 2026-10-28: a customer was quoted
// 0.0016980000280000002, which is float noise past XMR's 12 decimals.
// Quoted amounts must have at most the coin's decimals, in the stored
// value, in payment URIs, and in JSON, for every coin.
func TestRoundingToCoinDecimals(t *testing.T) {
	cases := []struct {
		coin string
		in   float64
		want string
	}{
		{"XMR", 0.0016980000280000002, "0.001698000028"},
		{"XMR", 0.123456789012345, "0.123456789012"},
		{"BTC", 0.010000000000000002, "0.01"},
		{"ETH", 0.12345678900000001, "0.123456789"},
		{"XLM", 0.12345679999999999, "0.1234568"},
		{"SOL", 2.0, "2"},
		{"ADA", 10.0, "10"},
	}
	for _, c := range cases {
		if got := fmtCoinAmount(c.coin, c.in); got != c.want {
			t.Errorf("%s fmtCoinAmount(%v) = %q, want %q", c.coin, c.in, got, c.want)
		}
		if got := fmtCoinAmount(c.coin, roundAmount(c.coin, c.in)); got != c.want {
			t.Errorf("%s roundAmount then fmt = %q, want %q", c.coin, got, c.want)
		}
		// JSON must carry the same clean number, not float noise.
		view := invoiceView{Coin: c.coin, AmountCrypto: c.in, TxIDs: []string{}}
		b, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"amount_crypto":`+c.want) {
			t.Errorf("%s invoice JSON = %s, want amount_crypto %s", c.coin, b, c.want)
		}
		// Payment URI must carry the same clean amount, for coins
		// whose URI amount is direct (ETH/ADA/AVAX convert to wei
		// or lovelace instead, so their URI is integer units).
		switch c.coin {
		case "XMR", "BTC", "SOL", "XLM":
			uri := paymentURI(&Config{}, &Invoice{Coin: c.coin, Address: "addr", AmountCrypto: c.in})
			if !strings.Contains(uri, c.want) {
				t.Errorf("%s payment URI = %q, want amount %s", c.coin, uri, c.want)
			}
		}
	}
}
