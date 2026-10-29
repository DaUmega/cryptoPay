package main

// JSON file store: invoices plus the address pool. One mutex guards all
// state; saves are atomic (write temp, fsync, rename). No database, no deps.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var acceptedCoins = []string{"BTC", "LTC", "ETH", "MATIC", "ARB", "OP", "BASE", "AVAX", "DOGE", "BCH", "DASH", "ZEC", "ADA", "XRP", "XLM", "TRX", "SOL", "USDC", "USDT", "XMR"}

func validCoin(coin string) bool {
	for _, c := range acceptedCoins {
		if c == coin {
			return true
		}
	}
	return false
}

type PoolEntry struct {
	Address   string    `json:"address"`
	Status    string    `json:"status"` // available, assigned, retired
	InvoiceID string    `json:"invoice_id,omitempty"`
	AddedAt   time.Time `json:"added_at"`
}

type Payment struct {
	TxID          string  `json:"txid"`
	Amount        float64 `json:"amount"`
	Confirmations int     `json:"confirmations"`
}

type Invoice struct {
	ID                string    `json:"id"`
	Coin              string    `json:"coin"`
	Address           string    `json:"address"`
	AmountCrypto      float64   `json:"amount_crypto"`
	BaseAmount        float64   `json:"base_amount"`
	Offset            float64   `json:"offset"`
	AmountUSD         float64   `json:"amount_usd,omitempty"`
	MerchantRef       string    `json:"merchant_ref,omitempty"`
	WebhookURL        string    `json:"webhook_url,omitempty"`
	ReturnURL         string    `json:"return_url,omitempty"`
	State             string    `json:"state"` // awaiting, detected, confirming, confirmed, expired, underpaid, paid_late
	Received          float64   `json:"received"`
	ConfirmedReceived float64   `json:"confirmed_received"`
	Payments          []Payment `json:"payments"`
	// Baseline: txids already present on the address when the invoice
	// was created, before the customer saw the address or amount.
	// Those are previous orders' payments, never this invoice's, so
	// watch must ignore them forever. Without this, a shared address
	// with history false-confirms every new invoice on its first poll.
	// BaselineDone false means the baseline listing failed at
	// creation; those invoices fall back to exact-match-only, which
	// is safe but drops partial and overpayment support.
	IgnoreTxIDs  []string `json:"ignore_txids,omitempty"`
	BaselineDone bool     `json:"baseline_done,omitempty"`
	// ADA only: balance already on the address at creation. The ADA
	// lister returns the whole balance, not per-tx amounts, so only
	// balance above this baseline counts as this invoice's payment.
	// Without it, a seller address with an existing balance
	// false-confirms every ADA invoice on its first poll.
	BaselineAmount  float64   `json:"baseline_amount,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	ConfirmedAt     time.Time `json:"confirmed_at,omitempty"`
	WebhookStatus   string    `json:"webhook_status,omitempty"` // pending, sent, failed
	WebhookAttempts int       `json:"webhook_attempts,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	Invoices map[string]*Invoice
	Pool     map[string][]*PoolEntry
}

func loadStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, Invoices: map[string]*Invoice{}, Pool: map[string][]*PoolEntry{}}
	for _, name := range []string{"invoices", "pool"} {
		path := filepath.Join(dir, name+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		if name == "invoices" {
			if err := json.Unmarshal(data, &s.Invoices); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		} else {
			if err := json.Unmarshal(data, &s.Pool); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	if s.Invoices == nil {
		s.Invoices = map[string]*Invoice{}
	}
	if s.Pool == nil {
		s.Pool = map[string][]*PoolEntry{}
	}
	return s, nil
}

// saveLocked writes both files atomically. Caller must hold s.mu.
// The temp file is fsynced before rename, so a crash cannot leave a
// half-written invoices file in place of a good one.
func (s *Store) saveLocked() error {
	for name, v := range map[string]any{"invoices": s.Invoices, "pool": s.Pool} {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		tmp := filepath.Join(s.dir, name+".json.tmp")
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(s.dir, name+".json")); err != nil {
			return err
		}
	}
	return nil
}

// addToPool stores new addresses. An address already in the pool is skipped,
// except a retired one, which an explicit re-add flips back to available
// (the operator checked it). Every address is format-validated first: a
// typo in a pool address used to create invoices that could never be
// paid, with no error until a customer complained.
func (s *Store) addToPool(coin string, addresses []string) (int, int, error) {
	if !validCoin(coin) {
		return 0, 0, fmt.Errorf("unknown coin %q", coin)
	}
	if len(addresses) > 1000 {
		return 0, 0, fmt.Errorf("at most 1000 addresses per request")
	}
	for _, addr := range addresses {
		if err := validateAddress(coin, strings.TrimSpace(addr)); err != nil {
			return 0, 0, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Snapshot first: if the save below fails, the in-memory pool
	// must look exactly like the API said it did (unchanged), not
	// like a successful add that will vanish on restart.
	before := make([]*PoolEntry, len(s.Pool[coin]))
	for i, e := range s.Pool[coin] {
		cp := *e
		before[i] = &cp
	}
	added, skipped := 0, 0
	for _, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		found := false
		for _, e := range s.Pool[coin] {
			if e.Address == addr {
				found = true
				if e.Status == "retired" {
					e.Status = "available"
					e.InvoiceID = ""
					added++
				} else {
					skipped++
				}
				break
			}
		}
		if !found {
			s.Pool[coin] = append(s.Pool[coin], &PoolEntry{Address: addr, Status: "available", AddedAt: time.Now()})
			added++
		}
	}
	if err := s.saveLocked(); err != nil {
		s.Pool[coin] = before
		return 0, 0, err
	}
	return added, skipped, nil
}

// Shared addresses: one pool address may serve many concurrent invoices
// for the same coin, distinguished by their quoted amounts (see watch.go).
// That is the old single-wallet model, with the amount tweak doing the
// distinguishing instead of a fresh address per invoice. So the pool
// never "runs out" while it holds at least one address for the coin.
// ADA shares too, distinguished by balance above a baseline recorded
// at creation (see invoice.go): its lister returns the whole balance
// under one synthetic txid, not per-transaction amounts, so the
// baseline, not the txid, is what separates old money from new.

// openUses reports whether any open invoice (awaiting, detected,
// confirming) uses the address. Used by free and retire so a shared
// address is never freed or retired out from under another invoice.
func (s *Store) openUses(address string) bool {
	for _, inv := range s.Invoices {
		if inv.Address != address {
			continue
		}
		if inv.State == "awaiting" || inv.State == "detected" || inv.State == "confirming" {
			return true
		}
	}
	return false
}

// free returns an assigned address to available, unless another open
// invoice still uses it (shared address). Used when invoice creation
// fails after assign: the address must not stay assigned for a
// non-existent invoice, and must not be retired (that would burn a
// good address on a transient save error).
func (s *Store) free(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openUses(address) {
		return
	}
	for _, entries := range s.Pool {
		for _, e := range entries {
			if e.Address == address && e.Status == "assigned" {
				e.Status = "available"
				e.InvoiceID = ""
			}
		}
	}
	_ = s.saveLocked()
}

// assign takes the oldest available address for a coin. If none is
// available, it reuses the oldest assigned address (shared model), or
// the oldest retired one. It errors only when the pool holds no
// address at all for the coin.
func (s *Store) assign(coin, invoiceID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Pool[coin] {
		if e.Status == "available" {
			e.Status = "assigned"
			e.InvoiceID = invoiceID
			if err := s.saveLocked(); err != nil {
				e.Status = "available"
				e.InvoiceID = ""
				return "", err
			}
			return e.Address, nil
		}
	}
	for _, e := range s.Pool[coin] {
		if e.Status == "assigned" {
			// Shared reuse: keep status assigned, do not overwrite
			// InvoiceID, which can only name one invoice. Amount
			// matching in watch.go, not this field, distinguishes
			// the invoices sharing the address.
			if err := s.saveLocked(); err != nil {
				return "", err
			}
			return e.Address, nil
		}
	}
	for _, e := range s.Pool[coin] {
		if e.Status == "retired" {
			e.Status = "assigned"
			e.InvoiceID = invoiceID
			if err := s.saveLocked(); err != nil {
				e.Status = "retired"
				e.InvoiceID = ""
				return "", err
			}
			return e.Address, nil
		}
	}
	return "", fmt.Errorf("no available %s addresses in pool; add more via POST /admin/pool", coin)
}

// retire moves an address out of circulation, unless another open
// invoice still uses it (shared address). A retired address is not
// dead: assign reuses it when no available or assigned address exists.
// The late-payment race is closed by amount reservation, not by
// retiring: amountInUse reserves an expired invoice's amount for its
// full 7-day late-payment window, so a new invoice on the same address
// cannot quote the same amount and steal the late payment.
func (s *Store) retire(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openUses(address) {
		return
	}
	for _, entries := range s.Pool {
		for _, e := range entries {
			if e.Address == address && e.Status == "assigned" {
				e.Status = "retired"
				e.InvoiceID = ""
			}
		}
	}
	_ = s.saveLocked()
}

func (s *Store) poolCounts(coin string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := map[string]int{"available": 0, "assigned": 0, "retired": 0}
	for _, e := range s.Pool[coin] {
		c[e.Status]++
	}
	return c
}
