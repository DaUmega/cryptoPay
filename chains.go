package main

// Per-coin payment listing: given a pool address, return the incoming
// payments seen for it (txid, amount in coin units, confirmations).
//
// Every lister returns only INCOMING payments to the address. Pool addresses
// are fresh, so the small result windows these APIs return (25 txs for
// Esplora, 20 for Tron/Solana/Blockbook) are plenty. Amounts are float64 coin units:
// exact to the offset unit for realistic values, and every acceptance check
// uses a 0.1% tolerance, so float noise never flips a decision.
//
// The `known` map (txid to stored amount) exists for Solana: its signature
// list does not carry amounts, so fetching the transaction for every known
// signature on every poll would be wasteful. Known signatures are returned
// with their stored amount and fresh confirmations; only new signatures
// trigger a transaction fetch. Other chains ignore `known` (their list
// responses already carry amounts).
//
// No lister takes an API key. EVM chains use Blockscout instances (ETH,
// Polygon, Arbitrum, Optimism, Base) or Routescan (Avalanche C-Chain),
// both Etherscan-compatible and keyless. UTXO coins besides BTC/LTC use
// Zelcore Blockbook instances, keyless. BNB was removed: it has no
// keyless Blockscout instance and its only explorer route is a keyed
// Etherscan API, which is exactly what this service no longer depends on.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (a *App) listPayments(coin, addr string, known map[string]float64) ([]Payment, error) {
	switch coin {
	case "BTC":
		return btcPayments(a.cfg.BTCExplorer, addr)
	case "LTC":
		return btcPayments(a.cfg.LTCExplorer, addr) // same Esplora shape, different chain
	case "ETH":
		return ethPayments(a.cfg.ETHAPI, addr)
	case "MATIC":
		return ethPayments(a.cfg.MATICAPI, addr)
	case "ARB":
		return ethPayments(a.cfg.ARBAPI, addr)
	case "OP":
		return ethPayments(a.cfg.OPAPI, addr)
	case "BASE":
		return ethPayments(a.cfg.BASEAPI, addr)
	case "AVAX":
		return ethPayments(a.cfg.AVAXAPI, addr)
	case "DOGE":
		return blockbookPayments(a.cfg.DOGEBlockbook, addr)
	case "BCH":
		return blockbookPayments(a.cfg.BCHBlockbook, addr)
	case "DASH":
		return blockbookPayments(a.cfg.DASHBlockbook, addr)
	case "ZEC":
		return blockbookPayments(a.cfg.ZECBlockbook, addr)
	case "ADA":
		return adaPayments(a.cfg, addr)
	case "XRP":
		return xrpPayments(a.cfg.XRPNode, addr)
	case "XLM":
		return xlmPayments(a.cfg.XLMHorizon, addr)
	case "TRX":
		return trxPayments(a.cfg.TronAPI, addr)
	case "SOL":
		return solPayments(a.cfg.SolRPC, addr, known)
	case "USDC":
		return solTokenPayments(a.cfg.SolRPC, addr, a.cfg.USDCMint, known)
	case "USDT":
		return solTokenPayments(a.cfg.SolRPC, addr, a.cfg.USDTMint, known)
	case "XMR":
		return a.xmrPayments(addr)
	}
	return nil, fmt.Errorf("unknown coin")
}

func btcPayments(explorer, addr string) ([]Payment, error) {
	var txs []struct {
		TxID   string `json:"txid"`
		Status struct {
			Confirmed   bool `json:"confirmed"`
			BlockHeight int  `json:"block_height"`
		} `json:"status"`
		Vout []struct {
			ScriptpubkeyAddress string `json:"scriptpubkey_address"`
			Value               int64  `json:"value"`
		} `json:"vout"`
	}
	if err := getJSON(explorer+"/address/"+addr+"/txs", &txs); err != nil {
		return nil, err
	}
	tip := 0
	for _, tx := range txs {
		if tx.Status.Confirmed {
			var h int
			if err := getJSON(explorer+"/blocks/tip/height", &h); err != nil {
				return nil, err
			}
			tip = h
			break
		}
	}
	var out []Payment
	for _, tx := range txs {
		var sats int64
		for _, v := range tx.Vout {
			if v.ScriptpubkeyAddress == addr {
				sats += v.Value
			}
		}
		if sats <= 0 {
			continue
		}
		conf := 0
		if tx.Status.Confirmed {
			conf = tip - tx.Status.BlockHeight + 1
			if conf < 1 {
				conf = 1
			}
		}
		out = append(out, Payment{TxID: tx.TxID, Amount: float64(sats) / 1e8, Confirmations: conf})
	}
	return out, nil
}

// ethPayments lists native-coin transfers via an Etherscan-compatible
// txlist endpoint (Blockscout or Routescan). No chainid or apikey is
// sent: Blockscout instances are per-chain, and Routescan encodes the
// chain in its URL path, so both work with the plain txlist query.
func ethPayments(api, addr string) ([]Payment, error) {
	url := api + "?module=account&action=txlist&address=" + addr + "&sort=desc"
	var r struct {
		Result []struct {
			Hash          string `json:"hash"`
			Confirmations string `json:"confirmations"`
			Value         string `json:"value"`
			To            string `json:"to"`
			IsError       string `json:"isError"`
		} `json:"result"`
	}
	if err := getJSON(url, &r); err != nil {
		return nil, err
	}
	var out []Payment
	for _, tx := range r.Result {
		if tx.IsError != "0" || !strings.EqualFold(tx.To, addr) {
			continue
		}
		wei, err := strconv.ParseFloat(tx.Value, 64)
		if err != nil || wei <= 0 {
			continue
		}
		conf, _ := strconv.Atoi(tx.Confirmations)
		out = append(out, Payment{TxID: tx.Hash, Amount: wei / 1e18, Confirmations: conf})
	}
	return out, nil
}

// blockbookPayments lists UTXO-coin payments via a Blockbook endpoint
// (DOGE, BCH, DASH, ZEC). Blockbook's address transactions carry vout
// addresses, values in base units, and a confirmations count per
// transaction. ZEC only works for transparent addresses: shielded
// outputs are not listed by Blockbook, which is why pool validation
// rejects shielded ZEC addresses outright.
func blockbookPayments(blockbook, addr string) ([]Payment, error) {
	if blockbook == "" {
		return nil, fmt.Errorf("no Blockbook URL configured for this coin")
	}
	var r struct {
		Transactions []struct {
			TxID          string `json:"txid"`
			BlockHeight   int    `json:"blockHeight"`
			Confirmations int    `json:"confirmations"`
			Vout          []struct {
				Addresses []string `json:"addresses"`
				Value     string   `json:"value"`
			} `json:"vout"`
		} `json:"transactions"`
	}
	if err := getJSON(strings.TrimSuffix(blockbook, "/")+"/api/v2/address/"+addr+"?details=txs&pageSize=20", &r); err != nil {
		return nil, err
	}
	var out []Payment
	for _, tx := range r.Transactions {
		var base float64
		for _, v := range tx.Vout {
			for _, a := range v.Addresses {
				if a == addr {
					val, _ := strconv.ParseFloat(v.Value, 64)
					base += val
				}
			}
		}
		if base <= 0 {
			continue
		}
		conf := tx.Confirmations
		if tx.BlockHeight <= 0 {
			conf = 0
		}
		out = append(out, Payment{TxID: tx.TxID, Amount: base / 1e8, Confirmations: conf})
	}
	return out, nil
}

// adaPayments detects via AdaStat's exact balance (keyless), then
// measures confirmations as the depth of the newest transaction via
// Koios (keyless, tier-limited). The whole balance counts as confirmed
// only when the newest payment is confirmed: conservative, and it keeps
// partial payments plus top-ups correct without per-UTXO tracking. If
// Koios does not answer, confirmations stay 0 and the invoice stays
// "detected" until it recovers; it never counts as confirmed on a
// balance alone. There is no Blockfrost path any more: that was the
// only keyed ADA route, and Koios covers it keyless.
func adaPayments(cfg *Config, addr string) ([]Payment, error) {
	bal, err := adaBalanceAdastat(cfg.AdaStatURL, addr)
	if err != nil {
		return nil, err
	}
	if bal <= 0 {
		return []Payment{}, nil
	}
	conf := 0
	if newest, tip, err := adaNewestBlock(cfg, addr); err == nil && newest > 0 && tip > 0 {
		conf = tip - newest + 1
		if conf < 0 {
			conf = 0
		}
	}
	return []Payment{{TxID: "adastat:" + addr, Amount: bal, Confirmations: conf}}, nil
}

func adaBalanceAdastat(base, addr string) (float64, error) {
	// AdaStat returns balance as a number in some responses and as
	// a string in others (seen live: unmarshal string into int64).
	// Accept both, otherwise every ADA invoice stays awaiting even
	// after a real payment, because the lister errors on every poll.
	var r struct {
		Data struct {
			Balance json.RawMessage `json:"balance"`
		} `json:"data"`
		Code int `json:"code"`
	}
	if err := getJSON(base+"/addresses/"+addr+".json", &r); err != nil {
		return 0, err
	}
	if r.Code != 200 || len(r.Data.Balance) == 0 || string(r.Data.Balance) == "null" {
		return 0, fmt.Errorf("adastat bad response (code %d)", r.Code)
	}
	var bal int64
	if err := json.Unmarshal(r.Data.Balance, &bal); err == nil {
		return float64(bal) / 1e6, nil
	}
	var s string
	if err := json.Unmarshal(r.Data.Balance, &s); err != nil {
		return 0, fmt.Errorf("adastat balance not a number or string: %s", r.Data.Balance)
	}
	bal, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("adastat balance %q: %w", s, err)
	}
	return float64(bal) / 1e6, nil
}

func adaNewestBlock(cfg *Config, addr string) (newest, tip int, err error) {
	var hashes []struct {
		TxHash string `json:"tx_hash"`
	}
	if err := postJSON(cfg.KoiosURL+"/address_txs", map[string]any{"_addresses": []string{addr}}, &hashes); err != nil {
		return 0, 0, err
	}
	if len(hashes) == 0 {
		return 0, 0, nil
	}
	list := make([]string, 0, len(hashes))
	for _, h := range hashes {
		list = append(list, h.TxHash)
	}
	var infos []struct {
		BlockHeight int `json:"block_height"`
	}
	if err := postJSON(cfg.KoiosURL+"/tx_info", map[string]any{"_tx_hashes": list}, &infos); err != nil {
		return 0, 0, err
	}
	for _, info := range infos {
		if info.BlockHeight > newest {
			newest = info.BlockHeight
		}
	}
	var tipResp []struct {
		BlockNo int `json:"block_no"`
	}
	if err := postJSON(cfg.KoiosURL+"/tip", map[string]any{}, &tipResp); err != nil {
		return 0, 0, err
	}
	if len(tipResp) > 0 {
		tip = tipResp[0].BlockNo
	}
	return newest, tip, nil
}

func xrpPayments(node, addr string) ([]Payment, error) {
	var r struct {
		Result struct {
			Error        string `json:"error"`
			Transactions []struct {
				Tx struct {
					Hash            string `json:"hash"`
					TransactionType string `json:"TransactionType"`
					Amount          any    `json:"Amount"`
					Destination     string `json:"Destination"`
				} `json:"tx"`
				LedgerIndex int  `json:"ledger_index"`
				Validated   bool `json:"validated"`
			} `json:"transactions"`
		} `json:"result"`
	}
	if err := postJSON(node, map[string]any{
		"method": "account_tx",
		"params": []any{map[string]any{"account": addr, "ledger_index_min": -1, "limit": 20}},
	}, &r); err != nil {
		return nil, err
	}
	if r.Result.Error == "actNotFound" {
		return []Payment{}, nil // unactivated address: no payments, not a failure
	}
	if r.Result.Error != "" {
		return nil, fmt.Errorf("xrpl: %s", r.Result.Error)
	}
	var out []Payment
	for _, t := range r.Result.Transactions {
		if t.Tx.TransactionType != "Payment" || t.Tx.Destination != addr {
			continue
		}
		drops, ok := t.Tx.Amount.(string)
		if !ok {
			continue // IOU token payment, not native XRP
		}
		d, err := strconv.ParseFloat(drops, 64)
		if err != nil || d <= 0 {
			continue
		}
		conf := 0
		if t.Validated {
			conf = 999 // XRP ledgers are final once validated (~4s)
		}
		out = append(out, Payment{TxID: t.Tx.Hash, Amount: d / 1e6, Confirmations: conf})
	}
	return out, nil
}

// xlmPayments lists native XLM payments via Stellar Horizon (keyless).
// Horizon only returns successful payments, and Stellar ledgers are
// final in about 5 seconds, so a listed payment counts as deeply
// confirmed (999, same sentinel as XRP) rather than 1, and the
// invoice's required count (default 1) is satisfied immediately.
func xlmPayments(horizon, addr string) ([]Payment, error) {
	var r struct {
		Embedded struct {
			Records []struct {
				Type            string `json:"type"`
				AssetType       string `json:"asset_type"`
				To              string `json:"to"`
				Amount          string `json:"amount"`
				TransactionHash string `json:"transaction_hash"`
			} `json:"records"`
		} `json:"_embedded"`
	}
	if err := getJSON(strings.TrimSuffix(horizon, "/")+"/accounts/"+addr+"/payments?limit=20&order=desc", &r); err != nil {
		return nil, err
	}
	var out []Payment
	for _, rec := range r.Embedded.Records {
		if rec.Type != "payment" || rec.AssetType != "native" || rec.To != addr {
			continue
		}
		amt, err := strconv.ParseFloat(rec.Amount, 64)
		if err != nil || amt <= 0 {
			continue
		}
		out = append(out, Payment{TxID: rec.TransactionHash, Amount: amt, Confirmations: 999})
	}
	return out, nil
}

func trxPayments(api, addr string) ([]Payment, error) {
	hexAddr, err := tronHex(addr)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data []struct {
			TxID        string `json:"txID"`
			BlockNumber int    `json:"blockNumber"`
			RawData     struct {
				Contract []struct {
					Type      string `json:"type"`
					Parameter struct {
						Value struct {
							Amount    int64  `json:"amount"`
							ToAddress string `json:"to_address"`
						} `json:"value"`
					} `json:"parameter"`
				} `json:"contract"`
			} `json:"raw_data"`
		} `json:"data"`
	}
	if err := getJSON(api+"/v1/accounts/"+addr+"/transactions?limit=20&order_by=block_timestamp,desc", &r); err != nil {
		return nil, err
	}
	var tipResp struct {
		BlockHeader struct {
			RawData struct {
				Number int `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := getJSON(api+"/wallet/getnowblock", &tipResp); err != nil {
		return nil, err
	}
	tip := tipResp.BlockHeader.RawData.Number
	var out []Payment
	for _, tx := range r.Data {
		for _, c := range tx.RawData.Contract {
			if c.Type != "TransferContract" {
				continue
			}
			toHex := strings.ToLower(strings.TrimPrefix(c.Parameter.Value.ToAddress, "0x"))
			if toHex != hexAddr || c.Parameter.Value.Amount <= 0 {
				continue
			}
			conf := 0
			if tx.BlockNumber > 0 && tip > 0 {
				conf = tip - tx.BlockNumber + 1
				if conf < 0 {
					conf = 0
				}
			}
			out = append(out, Payment{TxID: tx.TxID, Amount: float64(c.Parameter.Value.Amount) / 1e6, Confirmations: conf})
		}
	}
	return out, nil
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func solRPC(rpcURL, method string, params any) (json.RawMessage, error) {
	var r rpcResp
	if err := postJSON(rpcURL, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	}, &r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("solana: %s", r.Error.Message)
	}
	return r.Result, nil
}

// solStatus maps Solana commitment to a confirmation count: finalized is
// the rooted, irreversible state, so it counts as deeply confirmed.
func solStatus(s string) int {
	switch s {
	case "finalized":
		return 32
	case "confirmed":
		return 1
	}
	return 0
}

type solSig struct {
	Signature          string `json:"signature"`
	ConfirmationStatus string `json:"confirmationStatus"`
	Err                any    `json:"err"`
}

func solSignatures(rpc, addr string) ([]solSig, error) {
	raw, err := solRPC(rpc, "getSignaturesForAddress", []any{addr, map[string]any{"limit": 20}})
	if err != nil {
		return nil, err
	}
	var sigs []solSig
	if err := json.Unmarshal(raw, &sigs); err != nil {
		return nil, err
	}
	return sigs, nil
}

func solPayments(rpc, addr string, known map[string]float64) ([]Payment, error) {
	sigs, err := solSignatures(rpc, addr)
	if err != nil {
		return nil, err
	}
	var out []Payment
	for _, sig := range sigs {
		if sig.Err != nil {
			continue
		}
		conf := solStatus(sig.ConfirmationStatus)
		if amt, ok := known[sig.Signature]; ok {
			out = append(out, Payment{TxID: sig.Signature, Amount: amt, Confirmations: conf})
			continue
		}
		raw, err := solRPC(rpc, "getTransaction", []any{sig.Signature, map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0}})
		if err != nil {
			continue // not available yet; next poll retries
		}
		var tx struct {
			Meta struct {
				PreBalances  []int64 `json:"preBalances"`
				PostBalances []int64 `json:"postBalances"`
			} `json:"meta"`
			Transaction struct {
				Message struct {
					AccountKeys []string `json:"accountKeys"`
				} `json:"message"`
			} `json:"transaction"`
		}
		if err := json.Unmarshal(raw, &tx); err != nil {
			continue
		}
		for i, key := range tx.Transaction.Message.AccountKeys {
			if key == addr && i < len(tx.Meta.PreBalances) && i < len(tx.Meta.PostBalances) {
				delta := tx.Meta.PostBalances[i] - tx.Meta.PreBalances[i]
				if delta > 0 {
					out = append(out, Payment{TxID: sig.Signature, Amount: float64(delta) / 1e9, Confirmations: conf})
				}
				break
			}
		}
	}
	return out, nil
}

// solTokenPayments lists SPL token transfers (USDC, USDT) to addr.
// It finds addr's token account for mint, then walks that account's
// signatures the same way solPayments does for native SOL.
func solTokenPayments(rpc, addr, mint string, known map[string]float64) ([]Payment, error) {
	raw, err := solRPC(rpc, "getTokenAccountsByOwner", []any{
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
		return []Payment{}, nil // no token account yet: no token received
	}
	tokenAcct := r.Value[0].Pubkey
	sigs, err := solSignatures(rpc, tokenAcct)
	if err != nil {
		return nil, err
	}
	var out []Payment
	for _, sig := range sigs {
		if sig.Err != nil {
			continue
		}
		conf := solStatus(sig.ConfirmationStatus)
		if amt, ok := known[sig.Signature]; ok {
			out = append(out, Payment{TxID: sig.Signature, Amount: amt, Confirmations: conf})
			continue
		}
		raw, err := solRPC(rpc, "getTransaction", []any{sig.Signature, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0}})
		if err != nil {
			continue
		}
		var tx struct {
			Meta struct {
				PreTokenBalances []struct {
					Owner         string `json:"owner"`
					Mint          string `json:"mint"`
					UiTokenAmount struct {
						UiAmount float64 `json:"uiAmount"`
					} `json:"uiTokenAmount"`
				} `json:"preTokenBalances"`
				PostTokenBalances []struct {
					Owner         string `json:"owner"`
					Mint          string `json:"mint"`
					UiTokenAmount struct {
						UiAmount float64 `json:"uiAmount"`
					} `json:"uiTokenAmount"`
				} `json:"postTokenBalances"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(raw, &tx); err != nil {
			continue
		}
		var pre, post float64
		for _, b := range tx.Meta.PreTokenBalances {
			if b.Owner == addr && b.Mint == mint {
				pre += b.UiTokenAmount.UiAmount
			}
		}
		for _, b := range tx.Meta.PostTokenBalances {
			if b.Owner == addr && b.Mint == mint {
				post += b.UiTokenAmount.UiAmount
			}
		}
		if delta := post - pre; delta > 0 {
			out = append(out, Payment{TxID: sig.Signature, Amount: delta, Confirmations: conf})
		}
	}
	return out, nil
}
