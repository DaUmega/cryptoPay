package main

// Invoice creation: price conversion, tiny random offset, pool assignment,
// payment URI, and the state machine (derived from payments, not stored
// separately, except for the terminal states).
//
// The offset is what distinguishes concurrent invoices on a shared
// address: it is random 1-99 offset units, and must be unique among
// open (and recently expired) invoices for the coin, so no two invoices
// can quote the same amount and claim each other's payment.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// offsetUnit is the smallest step added as the random offset, per coin.
// Tiny in fiat terms (a few cents at most), but enough to make every quoted
// amount unique among open invoices for that coin.
func offsetUnit(coin string) float64 {
	switch coin {
	case "BTC", "LTC", "DOGE", "BCH", "DASH", "ZEC":
		return 1e-8 // 1 satoshi, litoshi, koinu, satoshi, duff, zatoshi
	case "ETH", "MATIC", "ARB", "OP", "BASE", "AVAX":
		return 1e-9 // 1 gwei
	case "ADA":
		return 1e-6 // 1 lovelace
	case "XRP":
		return 1e-6 // 1 drop
	case "XLM":
		return 1e-7 // 1 stroop
	case "TRX":
		return 1e-6 // 1 sun
	case "SOL":
		return 1e-9 // 1 lamport
	case "USDC", "USDT":
		return 1e-6 // 1 micro-unit
	case "XMR":
		return 1e-12 // 1 piconero
	}
	return 1e-6
}

// coinDecimals is the number of decimal places a quoted amount for
// this coin may have, matching its offset unit (1 satoshi = 8,
// 1 gwei/lamport = 9, 1 lovelace/drop/sun/micro-unit = 6,
// 1 stroop = 7, 1 piconero = 12). Anything beyond that is binary
// float noise, not money, and must never reach a customer.
func coinDecimals(coin string) int {
	switch coin {
	case "BTC", "LTC", "DOGE", "BCH", "DASH", "ZEC":
		return 8
	case "ETH", "MATIC", "ARB", "OP", "BASE", "AVAX", "SOL":
		return 9
	case "ADA", "XRP", "TRX", "USDC", "USDT":
		return 6
	case "XLM":
		return 7
	case "XMR":
		return 12
	}
	return 6
}

// roundAmount rounds amount to the coin's decimal places, via integer
// offset units, so base + offset never carries float noise past the
// last real unit (e.g. XMR 0.001698000028, not 0.0016980000280000002).
func roundAmount(coin string, amount float64) float64 {
	unit := offsetUnit(coin)
	return math.Round(amount/unit) * unit
}

// fmtCoinAmount formats amount with at most the coin's decimals,
// trimming trailing zeros, so clean amounts stay short (0.01, 2)
// and noisy amounts become clean (0.001698000028).
func fmtCoinAmount(coin string, amount float64) string {
	s := strconv.FormatFloat(amount, 'f', coinDecimals(coin), 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

func newInvoiceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (a *App) createInvoice(req createInvoiceReq) (*Invoice, error) {
	if !validCoin(req.Coin) {
		return nil, fmt.Errorf("unknown coin %q", req.Coin)
	}
	if len(req.MerchantRef) > 256 {
		return nil, fmt.Errorf("merchant_ref too long (max 256 chars)")
	}
	if err := validateHTTPURL(req.WebhookURL); err != nil {
		return nil, fmt.Errorf("webhook_url: %w", err)
	}
	if err := validateHTTPURL(req.ReturnURL); err != nil {
		return nil, fmt.Errorf("return_url: %w", err)
	}
	base := req.AmountCrypto
	amountUSD := req.AmountUSD
	if base > 0 && amountUSD > 0 {
		return nil, fmt.Errorf("pass amount_crypto or amount_usd, not both")
	}
	if base <= 0 {
		if amountUSD <= 0 {
			return nil, fmt.Errorf("pass amount_crypto, or amount_usd with a price source")
		}
		price, err := a.fetchPriceUSD(req.Coin)
		if err != nil {
			return nil, fmt.Errorf("price for %s: %w", req.Coin, err)
		}
		base = amountUSD / price
	}
	if base <= 0 || base > 1e9 || math.IsNaN(base) || math.IsInf(base, 0) {
		return nil, fmt.Errorf("invalid amount")
	}
	// Round the base to the coin's decimals first: a frontend quote
	// like USD/price, or amountUSD/price, carries float noise past
	// the last real unit, and adding the offset to it would bake
	// that noise into the amount the customer sees and must send.
	base = roundAmount(req.Coin, base)
	// Random offset, unique among open invoices for this coin.
	unit := offsetUnit(req.Coin)
	var quoted, offset float64
	for attempt := 0; attempt < 100; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(99))
		if err != nil {
			return nil, err
		}
		offset = float64(n.Int64()+1) * unit
		quoted = roundAmount(req.Coin, base+offset)
		if !a.amountInUse(req.Coin, quoted) {
			break
		}
		if attempt == 99 {
			return nil, fmt.Errorf("could not pick a unique offset for %s", req.Coin)
		}
	}
	id := newInvoiceID()
	addr, err := a.store.assign(req.Coin, id)
	if err != nil {
		return nil, err
	}
	// Baseline, before the customer sees the address or amount:
	// any payment already on the address cannot be this invoice's.
	// Best-effort: on lister failure, BaselineDone stays false and
	// watch falls back to exact-match-only for this invoice.
	// ADA uses a balance baseline instead of txids, handled below,
	// because its synthetic txid is not a real tx.
	var ignore []string
	baselineDone := false
	var baselineAmount float64
	if req.Coin == "ADA" {
		// ADA baseline is a balance, not txids (see store.go). If
		// the balance check fails, do not create the invoice at
		// all: with no baseline, the whole existing balance would
		// false-confirm it on the first poll.
		bal, err := adaBalanceAdastat(a.cfg.AdaStatURL, addr)
		if err != nil {
			a.store.free(addr)
			return nil, fmt.Errorf("ADA balance baseline failed: %w", err)
		}
		baselineAmount = bal
		baselineDone = true
	} else {
		// Baseline needs txids only, not amounts: for Solana, a full
		// listPayments fetches every transaction (up to 20 RPC calls,
		// slow enough to exceed the web backend's 30s timeout, which
		// orphans the invoice). Signature lists alone are 1-2 calls.
		if txids, err := a.baselineTxIDs(req.Coin, addr); err == nil {
			ignore = txids
			baselineDone = true
		} else {
			log.Printf("invoice %s %s baseline failed, exact-match-only: %v", id, req.Coin, err)
		}
	}
	now := time.Now()
	inv := &Invoice{
		ID: id, Coin: req.Coin, Address: addr,
		AmountCrypto: quoted, BaseAmount: base, Offset: offset,
		AmountUSD: amountUSD, MerchantRef: req.MerchantRef,
		WebhookURL: req.WebhookURL, ReturnURL: req.ReturnURL,
		IgnoreTxIDs: ignore, BaselineDone: baselineDone,
		BaselineAmount: baselineAmount,
		State:          "awaiting", CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(a.cfg.ExpiryMin) * time.Minute),
	}
	a.store.mu.Lock()
	a.store.Invoices[id] = inv
	if err := a.store.saveLocked(); err != nil {
		delete(a.store.Invoices, id)
		a.store.mu.Unlock()
		a.store.free(addr)
		return nil, err
	}
	a.store.mu.Unlock()
	return inv, nil
}

// baselineTxIDs returns the txids already on the address, for the
// invoice baseline. Solana is special-cased to signature lists only:
// full payment listing would fetch each transaction, which is slow
// and unnecessary when only txids are needed.
func (a *App) baselineTxIDs(coin, addr string) ([]string, error) {
	switch coin {
	case "SOL":
		sigs, err := solSignatures(a.cfg.SolRPC, addr)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, s := range sigs {
			if s.Signature != "" {
				out = append(out, s.Signature)
			}
		}
		return out, nil
	case "USDC", "USDT":
		mint := a.cfg.USDCMint
		if coin == "USDT" {
			mint = a.cfg.USDTMint
		}
		raw, err := solRPC(a.cfg.SolRPC, "getTokenAccountsByOwner", []any{
			addr, map[string]string{"mint": mint}, map[string]string{"encoding": "jsonParsed"},
		})
		if err != nil {
			return nil, err
		}
		var r struct {
			Value []struct {
				Pubkey string `json:"pubkey"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if len(r.Value) == 0 {
			return nil, nil
		}
		sigs, err := solSignatures(a.cfg.SolRPC, r.Value[0].Pubkey)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, s := range sigs {
			if s.Signature != "" {
				out = append(out, s.Signature)
			}
		}
		return out, nil
	}
	payments, err := a.listPayments(coin, addr, map[string]float64{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range payments {
		if p.TxID != "" {
			out = append(out, p.TxID)
		}
	}
	return out, nil
}

func (a *App) amountInUse(coin string, quoted float64) bool {
	// Two quoted amounts that differ by less than half an offset unit
	// are the same amount for this coin, given float64 noise. The old
	// code used a fixed 1e-12, which falsely collided distinct XMR
	// offsets (1 piconero apart) and was loose for large BTC amounts.
	// Amounts are reserved for open invoices and for expired/underpaid
	// ones still inside their 7-day late-payment window: a new invoice
	// quoting the same amount on a shared address could otherwise
	// steal the late payment, which still belongs to the old invoice.
	tol := offsetUnit(coin) / 2
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, inv := range a.store.Invoices {
		if inv.Coin != coin {
			continue
		}
		open := inv.State == "awaiting" || inv.State == "detected" || inv.State == "confirming"
		late := (inv.State == "expired" || inv.State == "underpaid") &&
			time.Now().Before(inv.ExpiresAt.Add(7*24*time.Hour))
		if !open && !late {
			continue
		}
		if absf(inv.AmountCrypto-quoted) < tol {
			return true
		}
	}
	return false
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func fmtAmount(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// fmtIntAmount formats an integer-unit amount (wei, lovelace) with
// no decimal point, regardless of float noise in the conversion.
func fmtIntAmount(f float64) string {
	return strconv.FormatFloat(math.Round(f), 'f', 0, 64)
}

// paymentURI is a wallet-friendly URI for the pay page and QR code. Not
// every coin has a standard URI (XRP, TRX, AVAX return ""), in which case
// the page shows the address and amount as text, which always works.
func paymentURI(cfg *Config, inv *Invoice) string {
	amt := fmtCoinAmount(inv.Coin, inv.AmountCrypto)
	switch inv.Coin {
	case "BTC":
		return "bitcoin:" + inv.Address + "?amount=" + amt
	case "LTC":
		return "litecoin:" + inv.Address + "?amount=" + amt
	case "DOGE":
		return "dogecoin:" + inv.Address + "?amount=" + amt
	case "BCH":
		// Pool addresses are stored with the bitcoincash: prefix, which
		// is also the URI scheme, so the address itself is the URI base.
		return inv.Address + "?amount=" + amt
	case "DASH":
		return "dash:" + inv.Address + "?amount=" + amt
	case "ZEC":
		return "zcash:" + inv.Address + "?amount=" + amt
	case "ETH", "MATIC", "ARB", "OP", "BASE":
		wei := fmtIntAmount(inv.AmountCrypto * 1e18)
		return "ethereum:" + inv.Address + "?value=" + wei
	case "AVAX":
		wei := fmtIntAmount(inv.AmountCrypto * 1e18)
		return "ethereum:" + inv.Address + "?value=" + wei
	case "ADA":
		lovelace := fmtIntAmount(inv.AmountCrypto * 1e6)
		return "cardano:" + inv.Address + "?amount=" + lovelace
	case "XLM":
		return "web+stellar:pay?destination=" + inv.Address + "&amount=" + amt
	case "SOL":
		return "solana:" + inv.Address + "?amount=" + amt + "&label=cryptopay"
	case "USDC":
		return "solana:" + inv.Address + "?amount=" + amt + "&spl-token=" + cfg.USDCMint + "&label=cryptopay"
	case "USDT":
		return "solana:" + inv.Address + "?amount=" + amt + "&spl-token=" + cfg.USDTMint + "&label=cryptopay"
	case "XMR":
		return "monero:" + inv.Address + "?amount=" + amt
	}
	return ""
}

type invoiceView struct {
	ID                    string    `json:"id"`
	Coin                  string    `json:"coin"`
	Address               string    `json:"address"`
	AmountCrypto          float64   `json:"amount_crypto"`
	AmountUSD             float64   `json:"amount_usd,omitempty"`
	MerchantRef           string    `json:"merchant_ref,omitempty"`
	State                 string    `json:"state"`
	Received              float64   `json:"received"`
	ConfirmedReceived     float64   `json:"confirmed_received"`
	Confirmations         int       `json:"confirmations"`
	ConfirmationsRequired int       `json:"confirmations_required"`
	TxIDs                 []string  `json:"txids"`
	PaymentURI            string    `json:"payment_uri,omitempty"`
	PayPath               string    `json:"pay_path"`
	PayURL                string    `json:"pay_url"`
	ExpiresAt             time.Time `json:"expires_at"`
	CreatedAt             time.Time `json:"created_at"`
	WebhookStatus         string    `json:"webhook_status,omitempty"`
}

// MarshalJSON outputs coin amounts as JSON numbers with at most
// the coin's decimals, so the API never returns float noise like
// 0.0016980000280000002. A plain float64 field would reintroduce
// that noise on marshal, even after roundAmount cleaned the value.
func (v invoiceView) MarshalJSON() ([]byte, error) {
	type rawView struct {
		ID                    string          `json:"id"`
		Coin                  string          `json:"coin"`
		Address               string          `json:"address"`
		AmountCrypto          json.RawMessage `json:"amount_crypto"`
		AmountUSD             float64         `json:"amount_usd,omitempty"`
		MerchantRef           string          `json:"merchant_ref,omitempty"`
		State                 string          `json:"state"`
		Received              json.RawMessage `json:"received"`
		ConfirmedReceived     json.RawMessage `json:"confirmed_received"`
		Confirmations         int             `json:"confirmations"`
		ConfirmationsRequired int             `json:"confirmations_required"`
		TxIDs                 []string        `json:"txids"`
		PaymentURI            string          `json:"payment_uri,omitempty"`
		PayPath               string          `json:"pay_path"`
		PayURL                string          `json:"pay_url"`
		ExpiresAt             time.Time       `json:"expires_at"`
		CreatedAt             time.Time       `json:"created_at"`
		WebhookStatus         string          `json:"webhook_status,omitempty"`
	}
	return json.Marshal(rawView{
		ID: v.ID, Coin: v.Coin, Address: v.Address,
		AmountCrypto:      json.RawMessage(fmtCoinAmount(v.Coin, v.AmountCrypto)),
		AmountUSD:         v.AmountUSD, MerchantRef: v.MerchantRef, State: v.State,
		Received:          json.RawMessage(fmtCoinAmount(v.Coin, v.Received)),
		ConfirmedReceived: json.RawMessage(fmtCoinAmount(v.Coin, v.ConfirmedReceived)),
		Confirmations: v.Confirmations, ConfirmationsRequired: v.ConfirmationsRequired,
		TxIDs: v.TxIDs, PaymentURI: v.PaymentURI, PayPath: v.PayPath, PayURL: v.PayURL,
		ExpiresAt: v.ExpiresAt, CreatedAt: v.CreatedAt, WebhookStatus: v.WebhookStatus,
	})
}

func invoiceResponse(cfg *Config, inv *Invoice) invoiceView {
	txids := []string{}
	maxConf := 0
	for _, p := range inv.Payments {
		txids = append(txids, p.TxID)
		if p.Confirmations > maxConf {
			maxConf = p.Confirmations
		}
	}
	payPath := "/pay/" + inv.ID
	payURL := payPath
	if cfg.PublicURL != "" {
		payURL = cfg.PublicURL + payPath
	}
	return invoiceView{
		ID: inv.ID, Coin: inv.Coin, Address: inv.Address,
		AmountCrypto: inv.AmountCrypto, AmountUSD: inv.AmountUSD,
		MerchantRef: inv.MerchantRef, State: inv.State,
		Received: inv.Received, ConfirmedReceived: inv.ConfirmedReceived,
		Confirmations: maxConf, ConfirmationsRequired: cfg.required(inv.Coin),
		TxIDs: txids, PaymentURI: paymentURI(cfg, inv),
		PayPath: payPath, PayURL: payURL,
		ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt,
		WebhookStatus: inv.WebhookStatus,
	}
}
