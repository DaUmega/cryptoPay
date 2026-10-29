package main

// Monero (XMR) via a view-only monero-wallet-rpc.
//
// Why a wallet RPC and not a public API: Monero's privacy means no
// explorer can tell you "did this subaddress receive funds" without your
// view key, because outputs are not linkable to an address on-chain. The
// wallet does the scanning locally (with the view key) and reports
// transfers; cryptopay just asks it. The wallet on the server is
// view-only, so it cannot spend even if compromised: the spend key
// never leaves the operator's full wallet. The wallet RPC talks to a
// daemon, which can be a public node picked from monero.fail (the wallet
// verifies transactions cryptographically, so a remote node can hide
// payments but not forge confirmed ones).
//
// Pool model: subaddresses. The operator generates subaddresses from
// their full wallet (one per invoice, or a batch) and adds them to the
// pool like any other coin's addresses. The view-only wallet tracks
// every subaddress of the account automatically. get_address_index maps
// a pool address to its (account, subaddress) index (cached; the mapping
// never changes), and get_transfers lists that subaddress's incoming
// and mempool transfers with amounts in atomic units and confirmations.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var xmrHTTP = &http.Client{Timeout: 15 * time.Second}

func (a *App) xmrRPC(method string, params any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", a.cfg.XMRWalletRPC, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.XMRWalletUser != "" || a.cfg.XMRWalletPass != "" {
		req.SetBasicAuth(a.cfg.XMRWalletUser, a.cfg.XMRWalletPass)
	}
	resp, err := xmrHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("wallet rpc %s: status %d", method, resp.StatusCode)
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("wallet rpc %s: %s", method, r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

func (a *App) xmrIndex(addr string) ([2]uint64, error) {
	a.xmrMu.Lock()
	if idx, ok := a.xmrIdx[addr]; ok {
		a.xmrMu.Unlock()
		return idx, nil
	}
	a.xmrMu.Unlock()
	var r struct {
		Index struct {
			Major uint64 `json:"major"`
			Minor uint64 `json:"minor"`
		} `json:"index"`
	}
	if err := a.xmrRPC("get_address_index", map[string]any{"address": addr}, &r); err != nil {
		return [2]uint64{}, err
	}
	idx := [2]uint64{r.Index.Major, r.Index.Minor}
	a.xmrMu.Lock()
	a.xmrIdx[addr] = idx
	a.xmrMu.Unlock()
	return idx, nil
}

func (a *App) xmrPayments(addr string) ([]Payment, error) {
	idx, err := a.xmrIndex(addr)
	if err != nil {
		return nil, err
	}
	var r struct {
		In []struct {
			TxID          string `json:"txid"`
			Amount        uint64 `json:"amount"`
			Confirmations uint64 `json:"confirmations"`
			Address       string `json:"address"`
		} `json:"in"`
		Pool []struct {
			TxID    string `json:"txid"`
			Amount  uint64 `json:"amount"`
			Address string `json:"address"`
		} `json:"pool"`
	}
	if err := a.xmrRPC("get_transfers", map[string]any{
		"in": true, "pool": true,
		"account_index": idx[0], "subaddr_indices": []uint64{idx[1]},
	}, &r); err != nil {
		return nil, err
	}
	var out []Payment
	for _, t := range r.In {
		if t.Amount == 0 {
			continue
		}
		out = append(out, Payment{TxID: t.TxID, Amount: float64(t.Amount) / 1e12, Confirmations: int(t.Confirmations)})
	}
	for _, t := range r.Pool {
		if t.Amount == 0 {
			continue
		}
		out = append(out, Payment{TxID: t.TxID, Amount: float64(t.Amount) / 1e12, Confirmations: 0})
	}
	return out, nil
}
