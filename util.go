package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

func parseBig(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	base := 10
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		value = value[2:]
		base = 16
	}
	n, ok := new(big.Int).SetString(value, base)
	if !ok {
		return nil, fmt.Errorf("invalid integer")
	}
	return n, nil
}

// decimalToUnits converts a non-negative decimal string such as "0.1" into
// integer base units with the given number of decimals. It rejects signs,
// more than one dot, a dot without digits on both sides (".", "5.", ".5"),
// and more fraction digits than decimals.
func decimalToUnits(value string, decimals uint8) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") {
		return nil, fmt.Errorf("invalid decimal amount")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return nil, fmt.Errorf("invalid decimal amount")
	}

	whole := parts[0]
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if whole == "" || fraction == "" {
			return nil, fmt.Errorf("invalid decimal amount")
		}
	}
	if len(fraction) > int(decimals) {
		return nil, fmt.Errorf("too many decimal places")
	}
	for _, ch := range whole + fraction {
		if ch < '0' || ch > '9' {
			return nil, fmt.Errorf("invalid decimal amount")
		}
	}

	fraction += strings.Repeat("0", int(decimals)-len(fraction))
	units, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal amount")
	}
	return units, nil
}

// tokenUnit is 10^decimals, the base units of one whole token.
func tokenUnit(decimals uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
}

// bpsDenominator is 100% expressed in basis points.
const bpsDenominator = 10_000

// applyBPSCeil returns ceil(value * bps / bpsDenominator); nil or zero inputs
// yield 0. Rounding up keeps fee and gas-buffer results on the safe side.
func applyBPSCeil(value *big.Int, bps uint64) *big.Int {
	if value == nil || value.Sign() == 0 || bps == 0 {
		return new(big.Int)
	}
	numerator := new(big.Int).Mul(value, new(big.Int).SetUint64(bps))
	numerator.Add(numerator, big.NewInt(bpsDenominator-1))
	return numerator.Div(numerator, big.NewInt(bpsDenominator))
}

func applyGasBuffer(estimate, bufferBPS uint64) (uint64, error) {
	buffered := applyBPSCeil(new(big.Int).SetUint64(estimate), bufferBPS)
	if !buffered.IsUint64() {
		return 0, errors.New("buffered gas limit overflows uint64")
	}
	return buffered.Uint64(), nil
}

// newID returns a random RFC 4122 UUIDv4: byte 6 carries version 4 in its high
// nibble and byte 8 the 10xx variant bits. It panics if crypto/rand fails.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// requestHash is the idempotency key of a withdraw request: SHA-256 over the
// job type, chain ID, and the lower-cased contract, proof, and args.
func requestHash(chainID string, req WithdrawRequest) string {
	parts := []string{
		"TORNADO_WITHDRAW",
		chainID,
		strings.ToLower(req.Contract),
		strings.ToLower(req.Proof),
	}
	for _, arg := range req.Args {
		parts = append(parts, strings.ToLower(arg))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
