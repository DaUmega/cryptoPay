package main

// Price lookup.
//
// Two sources, in order:
//   1. PRICE_API_URL, if set: the operator's own price API in the
//      cryptoPriceAPI shape (GET <base>/price/<SYMBOL> returns
//      {"price_usd": number}). This is the override for coins or
//      pricing the built-in source cannot cover.
//   2. Otherwise CoinPaprika, keyless: GET
//      https://api.coinpaprika.com/v1/tickers/<coin-id> returns
//      {"quotes":{"USD":{"price": number}}}. CoinPaprika needs no key,
//      is not geo-blocked from the places this was tested, and covers
//      every coin here, including XMR. CoinGecko was rejected as the
//      built-in source because its public endpoints now require a free
//      Demo key, and Binance because it geo-blocks whole regions.
//
// ARB, OP, and BASE pay in native ETH on their L2, not in ARB/OP
// tokens, so their price symbol is ETH. Getting this wrong used to
// quote an Arbitrum invoice at the ARB token price instead of ETH.
// MATIC keeps its old code; its gas token is now POL, priced here via
// CoinPaprika's matic-polygon ticker, which tracks that migration.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func doReq(req *http.Request) ([]byte, error) {
	req.Header.Set("User-Agent", "cryptopay/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s %s: status %d", req.Method, req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

func getJSON(url string, out any) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	body, err := doReq(req)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func getJSONHeaders(url string, headers map[string]string, out any) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	body, err := doReq(req)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func postJSON(url string, payload any, out any) error {
	return postJSONHeaders(url, payload, nil, out)
}

func postJSONHeaders(url string, payload any, headers map[string]string, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	body, err := doReq(req)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// priceCoinID maps a cryptopay coin to its CoinPaprika ticker id, or
// "" when the built-in source cannot price it (none today: every coin
// in acceptedCoins is mapped, which the test enforces).
func priceCoinID(coin string) string {
	switch coin {
	case "BTC":
		return "btc-bitcoin"
	case "LTC":
		return "ltc-litecoin"
	case "ETH", "ARB", "OP", "BASE":
		return "eth-ethereum"
	case "MATIC":
		return "matic-polygon"
	case "AVAX":
		return "avax-avalanche"
	case "DOGE":
		return "doge-dogecoin"
	case "BCH":
		return "bch-bitcoin-cash"
	case "DASH":
		return "dash-dash"
	case "ZEC":
		return "zec-zcash"
	case "ADA":
		return "ada-cardano"
	case "XRP":
		return "xrp-xrp"
	case "XLM":
		return "xlm-stellar"
	case "TRX":
		return "trx-tron"
	case "SOL":
		return "sol-solana"
	case "USDC":
		return "usdc-usd-coin"
	case "USDT":
		return "usdt-tether"
	case "XMR":
		return "xmr-monero"
	}
	return ""
}

// priceSymbol is the symbol a price API should be asked for. ARB, OP,
// and BASE pay in native ETH on their L2, so asking for "ARB" would
// quote an Arbitrum invoice at the ARB token price instead of ETH.
// This applies to both price sources, legacy and built-in.
func priceSymbol(coin string) string {
	switch coin {
	case "ARB", "OP", "BASE":
		return "ETH"
	}
	return coin
}

func (a *App) fetchPriceUSD(sym string) (float64, error) {
	if a.cfg.PriceAPIURL != "" {
		var r struct {
			PriceUSD float64 `json:"price_usd"`
		}
		base := strings.TrimSuffix(a.cfg.PriceAPIURL, "/")
		if err := getJSON(base+"/price/"+priceSymbol(sym), &r); err != nil {
			return 0, err
		}
		if r.PriceUSD <= 0 || math.IsNaN(r.PriceUSD) || math.IsInf(r.PriceUSD, 0) {
			return 0, fmt.Errorf("no price for %s", sym)
		}
		return r.PriceUSD, nil
	}
	id := priceCoinID(sym)
	if id == "" {
		return 0, fmt.Errorf("no built-in price for %s; set PRICE_API_URL", sym)
	}
	base := strings.TrimSuffix(a.cfg.PriceBaseURL, "/")
	if base == "" {
		base = "https://api.coinpaprika.com"
	}
	var r struct {
		Symbol string `json:"symbol"`
		Quotes struct {
			USD struct {
				Price float64 `json:"price"`
			} `json:"USD"`
		} `json:"quotes"`
	}
	if err := getJSON(base+"/v1/tickers/"+id, &r); err != nil {
		return 0, err
	}
	if r.Quotes.USD.Price <= 0 || math.IsNaN(r.Quotes.USD.Price) || math.IsInf(r.Quotes.USD.Price, 0) {
		return 0, fmt.Errorf("no price for %s", sym)
	}
	return r.Quotes.USD.Price, nil
}
