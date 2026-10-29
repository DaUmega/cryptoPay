package main

// Watch loop: poll every open invoice's pool address, merge newly seen
// payments, derive the state, fire the webhook on confirmation.
//
// Shared addresses: several invoices may watch the same address at
// once, distinguished by their quoted amounts, and by a baseline:
// at invoice creation, every txid already on the address is recorded
// and ignored forever, because it is a previous order's payment.
// A listed payment is claimed by an invoice only if it is not in
// that baseline, and it matches that invoice: a full match (within
// half an offset unit), or, with a baseline, a partial or top-up
// that fits the remaining amount, or an overpayment, and only if no
// other open invoice on the same address matches it in full (full
// matches take priority, deterministically, regardless of poll
// order) and no other invoice already claimed that txid. A payment
// matching no invoice is ignored, never double-counted. ADA is the
// exception: its lister returns the whole balance under one
// synthetic txid, not per-tx amounts, so it cannot distinguish
// invoices on one address; ADA shares too, via a baseline balance
// recorded at creation (see invoice.go).
//
// Expiry rules:
//   - No payment by expiry: expired, address retired unless another
//     open invoice still uses it.
//   - Partial payment still short 24h after expiry: underpaid, same.
//   - A payment that is detected but still confirming never expires on its
//     own: the money is real and on its way, so it keeps polling.
//   - Expired and underpaid invoices keep polling for 7 days after expiry,
//     so a late payment is never silently ignored: a fully paid late
//     invoice becomes paid_late and fires its webhook for manual review.
//     Their quoted amounts stay reserved for that window (amountInUse),
//     so a new invoice on the same address cannot steal the late payment.
//
// Polling is bounded-parallel (8 workers): the old serial loop let one
// slow or hanging chain API stall every other invoice for its full
// 15-second timeout, every poll cycle. A lister error still never
// changes state: the invoice simply stays where it was until the next
// poll, so a bad API response can delay a confirmation but never
// invent one.

import (
	"log"
	"math"
	"sync"
	"time"
)

func deriveOpenState(received, confirmedRecv, amount float64, maxConf int) string {
	switch {
	case confirmedRecv >= amount*0.999:
		return "confirmed"
	case received <= 0:
		return "awaiting"
	case maxConf == 0:
		return "detected"
	default:
		return "confirming"
	}
}

func (a *App) watchLoop() {
	a.checkAll()
	ticker := time.NewTicker(time.Duration(a.cfg.PollSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.checkAll()
	}
}

func (a *App) checkAll() {
	a.store.mu.Lock()
	var list []*Invoice
	for _, inv := range a.store.Invoices {
		switch inv.State {
		case "awaiting", "detected", "confirming":
			list = append(list, inv)
		case "expired", "underpaid":
			if time.Now().Before(inv.ExpiresAt.Add(7 * 24 * time.Hour)) {
				list = append(list, inv)
			}
		}
	}
	a.store.mu.Unlock()
	jobs := make(chan *Invoice)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for inv := range jobs {
				a.checkInvoice(inv)
			}
		}()
	}
	for _, inv := range list {
		jobs <- inv
	}
	close(jobs)
	wg.Wait()
}

func (a *App) checkInvoice(inv *Invoice) {
	a.store.mu.Lock()
	cur := a.store.Invoices[inv.ID]
	if cur == nil || cur.State == "confirmed" || cur.State == "paid_late" {
		a.store.mu.Unlock()
		return
	}
	open := cur.State == "awaiting" || cur.State == "detected" || cur.State == "confirming"
	now := time.Now()
	if open && cur.Received == 0 && now.After(cur.ExpiresAt) {
		cur.State = "expired"
		_ = a.store.saveLocked()
		a.store.mu.Unlock()
		a.store.retire(cur.Address)
		return
	}
	if open && cur.Received > 0 && cur.Received < cur.AmountCrypto*0.999 && now.After(cur.ExpiresAt.Add(24*time.Hour)) {
		cur.State = "underpaid"
		_ = a.store.saveLocked()
		a.store.mu.Unlock()
		a.store.retire(cur.Address)
		return
	}
	known := map[string]float64{}
	for _, p := range cur.Payments {
		known[p.TxID] = p.Amount
	}
	addr, coin := cur.Address, cur.Coin
	a.store.mu.Unlock()

	payments, err := a.listPayments(coin, addr, known)
	if err != nil {
		log.Printf("watch %s %s: %v", inv.ID, coin, err)
		return
	}
	// ADA: the lister returns the whole balance, so subtract the
	// baseline balance recorded at creation. Only new funds count.
	if coin == "ADA" && cur.BaselineDone {
		var filtered []Payment
		for _, p := range payments {
			if p.Amount > cur.BaselineAmount {
				p.Amount -= cur.BaselineAmount
				filtered = append(filtered, p)
			}
		}
		payments = filtered
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	cur = a.store.Invoices[inv.ID]
	if cur == nil || cur.State == "confirmed" || cur.State == "paid_late" {
		return
	}
	byID := map[string]int{}
	for i, p := range cur.Payments {
		byID[p.TxID] = i
	}
	changed := false
	running := cur.Received // attributed total before this poll
	for _, p := range payments {
		// A lister must never be able to confirm an invoice with a
		// bogus payment: empty txid, zero/negative, or non-finite
		// amounts are dropped here, regardless of what the API said.
		if p.TxID == "" || p.Amount <= 0 || math.IsNaN(p.Amount) || math.IsInf(p.Amount, 0) {
			continue
		}
		if i, ok := byID[p.TxID]; ok {
			if cur.Payments[i].Confirmations != p.Confirmations || cur.Payments[i].Amount != p.Amount {
				cur.Payments[i].Confirmations = p.Confirmations
				cur.Payments[i].Amount = p.Amount
				changed = true
			}
			continue
		}
		// New payment: claim it only if it belongs to this invoice.
		// ADA skips this: its balance lister cannot distinguish
		// invoices, and ADA uses a baseline balance instead, so the
		// sum-everything behaviour is correct there once the baseline
		// is subtracted in checkInvoice.
		if cur.Coin != "ADA" && !a.claimsPayment(cur, p, running) {
			continue
		}
		cur.Payments = append(cur.Payments, p)
		byID[p.TxID] = len(cur.Payments) - 1
		running += p.Amount
		changed = true
	}
	if !changed {
		return
	}
	var received, confirmedRecv float64
	var maxConf int
	for _, p := range cur.Payments {
		received += p.Amount
		if p.Confirmations >= a.cfg.required(cur.Coin) {
			confirmedRecv += p.Amount
		}
		if p.Confirmations > maxConf {
			maxConf = p.Confirmations
		}
	}
	cur.Received, cur.ConfirmedReceived = received, confirmedRecv
	oldState := cur.State
	if open {
		cur.State = deriveOpenState(received, confirmedRecv, cur.AmountCrypto, maxConf)
		if cur.State == "confirmed" {
			cur.ConfirmedAt = time.Now()
		}
	} else if confirmedRecv >= cur.AmountCrypto*0.999 {
		cur.State = "paid_late" // late payment to an expired invoice
		cur.ConfirmedAt = time.Now()
	}
	_ = a.store.saveLocked()
	if (cur.State == "confirmed" || cur.State == "paid_late") && oldState != cur.State && cur.WebhookURL != "" {
		cur.WebhookStatus = "pending"
		_ = a.store.saveLocked()
		go a.fireWebhook(cur)
	}
}

// claimsPayment reports whether payment p, not yet in cur's list,
// belongs to cur. Caller must hold a.store.mu: the checks below read
// other invoices sharing cur's address, and the merge that follows
// runs under the same lock, so two invoices cannot claim one tx.
// tol is half an offset unit: quoted amounts for one coin differ by
// at least a full offset unit, so a payment can fully match at most
// one open invoice on the address.
//
// The critical rule, learned from a live false confirmation: a
// payment that existed before the invoice was created (in
// cur.IgnoreTxIDs) is never claimed, even if its amount happens to
// match, because it is a previous order's payment on a shared
// address. Without a baseline, partial and overpayment claiming
// would claim every old transaction on the address and confirm
// the invoice with no new payment at all, which is exactly what
// happened on the seller addresses (their history summed to more
// than every new invoice's amount). So:
//   - baseline txid: never claim, period.
//   - no baseline (listing failed at creation): exact-match-only.
//     Old txs rarely match exactly, so this is safe, but partials
//     and overpayments are dropped for that invoice.
//   - with baseline: full match, partial/top-up, and overpayment
//     are all safe, because any tx not in the baseline arrived
//     after the invoice was disclosed to the customer, so it
//     cannot be someone else's old payment. A tx that full-matches
//     another open or late invoice on the same address is still
//     never claimed as a partial or overpayment for this one.
func (a *App) claimsPayment(cur *Invoice, p Payment, running float64) bool {
	for _, txid := range cur.IgnoreTxIDs {
		if txid == p.TxID {
			return false // pre-existing payment, not this invoice's
		}
	}
	tol := offsetUnit(cur.Coin) / 2
	for _, other := range a.store.Invoices {
		if other.ID == cur.ID || other.Coin != cur.Coin || other.Address != cur.Address {
			continue
		}
		for _, op := range other.Payments {
			if op.TxID == p.TxID {
				return false // already claimed by another invoice
			}
		}
		open := other.State == "awaiting" || other.State == "detected" || other.State == "confirming"
		late := (other.State == "expired" || other.State == "underpaid") &&
			time.Now().Before(other.ExpiresAt.Add(7*24*time.Hour))
		if (open || late) && absf(other.AmountCrypto-p.Amount) <= tol {
			return false // full match belongs to the other invoice
		}
	}
	if absf(p.Amount-cur.AmountCrypto) <= tol {
		return true // full payment, exact quoted amount
	}
	if !cur.BaselineDone {
		return false // no baseline: do not trust partials or overpayments
	}
	if running+p.Amount <= cur.AmountCrypto+tol {
		return true // partial or top-up that fits the remaining amount
	}
	if running+p.Amount >= cur.AmountCrypto-tol {
		return true // top-up that overshoots, or a plain overpayment
	}
	return false
}
