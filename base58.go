package main

// Base58 decode, needed only for Tron: Trongrid reports transfer recipients
// as hex, while pool addresses are base58. Decode the pool address and
// compare hex to hex. No checksum validation: comparison, not spending.

import (
	"fmt"
	"math/big"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Decode(s string) ([]byte, error) {
	n := big.NewInt(0)
	for _, c := range s {
		i := -1
		for j, a := range base58Alphabet {
			if a == c {
				i = j
				break
			}
		}
		if i < 0 {
			return nil, fmt.Errorf("bad base58 char %q", c)
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	decoded := n.Bytes()
	// Leading '1's are leading zero bytes.
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	out := make([]byte, zeros+len(decoded))
	copy(out[zeros:], decoded)
	return out, nil
}

// tronHex converts a base58 Tron address to its hex form (lowercase, no
// 0x prefix), as Trongrid reports it. A Tron address decodes to 25 bytes:
// 0x41 prefix, 20 address bytes, 4 checksum bytes. The hex form keeps the
// 0x41 prefix, so it is 21 bytes (42 hex chars).
func tronHex(addr string) (string, error) {
	raw, err := base58Decode(addr)
	if err != nil {
		return "", err
	}
	if len(raw) != 25 || raw[0] != 0x41 {
		return "", fmt.Errorf("not a tron address")
	}
	return fmt.Sprintf("%x", raw[0:21]), nil
}
