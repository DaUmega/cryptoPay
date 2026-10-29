package main

// Webhook delivery: HMAC-SHA256 over the body, retries with backoff. The
// merchant verifies with the shared WEBHOOK_SECRET. No secret means no
// signature header (local testing only; the README says so plainly).

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

type webhookPayload struct {
	InvoiceID    string    `json:"invoice_id"`
	MerchantRef  string    `json:"merchant_ref,omitempty"`
	Coin         string    `json:"coin"`
	Address      string    `json:"address"`
	AmountCrypto float64   `json:"amount_crypto"`
	AmountUSD    float64   `json:"amount_usd,omitempty"`
	State        string    `json:"state"` // confirmed, or paid_late
	TxIDs        []string  `json:"txids"`
	ConfirmedAt  time.Time `json:"confirmed_at,omitempty"`
}

// MarshalJSON mirrors invoiceView: coin amounts as JSON numbers with
// at most the coin's decimals, never float noise.
func (p webhookPayload) MarshalJSON() ([]byte, error) {
	type rawPayload struct {
		InvoiceID    string          `json:"invoice_id"`
		MerchantRef  string          `json:"merchant_ref,omitempty"`
		Coin         string          `json:"coin"`
		Address      string          `json:"address"`
		AmountCrypto json.RawMessage `json:"amount_crypto"`
		AmountUSD    float64         `json:"amount_usd,omitempty"`
		State        string          `json:"state"`
		TxIDs        []string        `json:"txids"`
		ConfirmedAt  time.Time       `json:"confirmed_at,omitempty"`
	}
	return json.Marshal(rawPayload{
		InvoiceID: p.InvoiceID, MerchantRef: p.MerchantRef, Coin: p.Coin,
		Address: p.Address, AmountCrypto: json.RawMessage(fmtCoinAmount(p.Coin, p.AmountCrypto)),
		AmountUSD: p.AmountUSD, State: p.State, TxIDs: p.TxIDs, ConfirmedAt: p.ConfirmedAt,
	})
}

func signWebhook(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) fireWebhook(inv *Invoice) {
	txids := []string{}
	for _, p := range inv.Payments {
		txids = append(txids, p.TxID)
	}
	body, err := json.Marshal(webhookPayload{
		InvoiceID: inv.ID, MerchantRef: inv.MerchantRef, Coin: inv.Coin,
		Address: inv.Address, AmountCrypto: inv.AmountCrypto,
		AmountUSD: inv.AmountUSD, State: inv.State, TxIDs: txids,
		ConfirmedAt: inv.ConfirmedAt,
	})
	if err != nil {
		log.Printf("webhook %s: marshal: %v", inv.ID, err)
		return
	}
	sig := signWebhook(a.cfg.WebhookSecret, body)
	delays := []time.Duration{0, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	for i, d := range delays {
		if d > 0 {
			time.Sleep(d)
		}
		req, err := http.NewRequest("POST", inv.WebhookURL, bytes.NewReader(body))
		if err != nil {
			break
		}
		req.Header.Set("Content-Type", "application/json")
		if sig != "" {
			req.Header.Set("X-Cryptopay-Signature", sig)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				a.store.mu.Lock()
				if cur := a.store.Invoices[inv.ID]; cur != nil {
					cur.WebhookStatus = "sent"
					cur.WebhookAttempts = i + 1
					_ = a.store.saveLocked()
				}
				a.store.mu.Unlock()
				return
			}
		}
	}
	a.store.mu.Lock()
	if cur := a.store.Invoices[inv.ID]; cur != nil {
		cur.WebhookStatus = "failed"
		cur.WebhookAttempts = len(delays)
		_ = a.store.saveLocked()
	}
	a.store.mu.Unlock()
}
