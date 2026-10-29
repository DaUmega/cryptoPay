package main

// Input validation: the two places untrusted or typo-prone input enters
// the service are the address pool (operator typos) and invoice creation
// (webhook/return URLs, amounts, refs, all supplied by the token holder's
// backend). Rejecting bad input here, with a clear error, is what keeps a
// typo from becoming an invoice that can never be paid, and a javascript:
// URL from becoming a redirect target on the pay page.
//
// Address checks are format checks only (prefix, length, charset), not
// checksum checks, except Tron, where the base58 decode already proves
// the 25-byte shape. A format-valid address can still be the wrong
// wallet; nothing here can detect that, which is why the pool is
// operator-supplied in the first place.

import (
	"fmt"
	"net/url"
	"strings"
)

const base58Chars = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
const bech32Chars = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
const base32Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

func allChars(s, allowed string) bool {
	for _, c := range s {
		if !strings.ContainsRune(allowed, c) {
			return false
		}
	}
	return true
}

func isBase58(s string) bool { return s != "" && allChars(s, base58Chars) }

// validateAddress reports whether addr looks like a receiving address
// for coin. Unknown coins are rejected, so adding a coin without adding
// its validator here is a loud failure, not a silent accept.
func validateAddress(coin, addr string) error {
	if addr == "" {
		return fmt.Errorf("empty address for %s", coin)
	}
	switch coin {
	case "BTC":
		if strings.HasPrefix(addr, "bc1q") || strings.HasPrefix(addr, "bc1p") {
			if (len(addr) == 42 || len(addr) == 62) && allChars(addr[3:], bech32Chars) {
				return nil
			}
			return fmt.Errorf("invalid BTC bech32 address %q", addr)
		}
		if (addr[0] == '1' || addr[0] == '3') && len(addr) >= 26 && len(addr) <= 35 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid BTC address %q", addr)
	case "LTC":
		if strings.HasPrefix(addr, "ltc1") {
			if (len(addr) == 43 || len(addr) == 63) && allChars(addr[4:], bech32Chars) {
				return nil
			}
			return fmt.Errorf("invalid LTC bech32 address %q", addr)
		}
		if (addr[0] == 'L' || addr[0] == 'M') && len(addr) >= 26 && len(addr) <= 35 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid LTC address %q", addr)
	case "ETH", "MATIC", "ARB", "OP", "BASE", "AVAX":
		if len(addr) == 42 && strings.HasPrefix(addr, "0x") && allChars(addr[2:], "0123456789abcdefABCDEF") {
			return nil
		}
		return fmt.Errorf("invalid %s address %q (want 0x plus 40 hex chars)", coin, addr)
	case "DOGE":
		if addr[0] == 'D' && len(addr) == 34 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid DOGE address %q", addr)
	case "BCH":
		// Cashaddr with prefix only. Blockbook rejects bare cashaddr
		// (no prefix) and normalises legacy addresses to cashaddr, so
		// accepting legacy here would create invoices Blockbook
		// could not match. Require the form Blockbook returns.
		if strings.HasPrefix(addr, "bitcoincash:") {
			rest := addr[len("bitcoincash:"):]
			if (rest[0] == 'q' || rest[0] == 'p') && len(rest) == 42 && allChars(rest[1:], bech32Chars) {
				return nil
			}
		}
		return fmt.Errorf("invalid BCH address %q (want bitcoincash:q... cashaddr)", addr)
	case "DASH":
		if addr[0] == 'X' && len(addr) == 34 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid DASH address %q", addr)
	case "ZEC":
		if (strings.HasPrefix(addr, "t1") || strings.HasPrefix(addr, "t3")) && len(addr) == 35 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid ZEC transparent address %q (shielded addresses cannot be watched)", addr)
	case "ADA":
		if strings.HasPrefix(addr, "addr1") && len(addr) >= 50 && len(addr) <= 110 && allChars(addr, "abcdefghijklmnopqrstuvwxyz0123456789") {
			return nil
		}
		return fmt.Errorf("invalid ADA address %q", addr)
	case "XRP":
		if addr[0] == 'r' && len(addr) >= 25 && len(addr) <= 35 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid XRP address %q", addr)
	case "XLM":
		if addr[0] == 'G' && len(addr) == 56 && allChars(addr, base32Chars) {
			return nil
		}
		return fmt.Errorf("invalid XLM address %q (want G plus 55 base32 chars)", addr)
	case "TRX":
		if _, err := tronHex(addr); err != nil {
			return fmt.Errorf("invalid TRX address %q", addr)
		}
		return nil
	case "SOL", "USDC", "USDT":
		if len(addr) >= 32 && len(addr) <= 44 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid %s address %q", coin, addr)
	case "XMR":
		// Primary addresses start with 4, subaddresses with 8; both are
		// 95 chars. The pool uses subaddresses (the view-only wallet
		// tracks every subaddress of its account automatically), so
		// rejecting 8... would reject every real pool subaddress.
		if (addr[0] == '4' || addr[0] == '8') && len(addr) == 95 && isBase58(addr) {
			return nil
		}
		return fmt.Errorf("invalid XMR address %q (want a 95-char address starting with 4 or 8)", addr)
	}
	return fmt.Errorf("unknown coin %q", coin)
}

// validateHTTPURL accepts only http/https URLs with a host, at most
// 2048 chars, no spaces or control characters. That rules out
// javascript:, data:, file:, and other schemes that must never reach
// a webhook POST or a pay-page redirect. It deliberately does not block
// private or loopback hosts: a merchant backend on the same machine is
// a normal webhook target, and the URL comes from the token holder,
// not from an untrusted customer.
func validateHTTPURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("invalid URL %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid URL %q (want http or https with a host)", raw)
	}
	return nil
}

// validInvoiceID reports whether id looks like an ID this service
// minted: 32 lowercase hex chars. Anything else is a 404, never a
// store lookup, so odd paths cannot probe the store.
func validInvoiceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	return allChars(id, "0123456789abcdef")
}
