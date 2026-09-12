// Package usage implements host-owned resource accounting. It retains cumulative
// endpoints and bounded interval summaries, never a sequence of samples.
package usage

import (
	"encoding/json"
	"errors"
	"math/big"
	"math/bits"
)

var ErrOverflow = errors.New("usage: integer overflow")

// Uint128 is an unsigned integer. JSON uses a decimal string, including for
// small values, so a consumer never loses precision through a JSON number.
type Uint128 struct{ Hi, Lo uint64 }

func (a Uint128) Add(b Uint128) (Uint128, error) {
	lo, carry := bits.Add64(a.Lo, b.Lo, 0)
	hi, carry := bits.Add64(a.Hi, b.Hi, carry)
	if carry != 0 {
		return Uint128{}, ErrOverflow
	}
	return Uint128{hi, lo}, nil
}

func (a Uint128) Sub(b Uint128) (Uint128, error) {
	lo, borrow := bits.Sub64(a.Lo, b.Lo, 0)
	hi, borrow := bits.Sub64(a.Hi, b.Hi, borrow)
	if borrow != 0 {
		return Uint128{}, errors.New("usage: decreasing integer")
	}
	return Uint128{hi, lo}, nil
}

func product(a, b uint64) Uint128 { hi, lo := bits.Mul64(a, b); return Uint128{hi, lo} }

func (a Uint128) div(d uint64) (Uint128, uint64) {
	hi, rem := a.Hi/d, a.Hi%d
	lo, rem := bits.Div64(rem, a.Lo, d)
	return Uint128{hi, lo}, rem
}

func (a Uint128) String() string {
	n := new(big.Int).SetUint64(a.Hi)
	n.Lsh(n, 64).Add(n, new(big.Int).SetUint64(a.Lo))
	return n.String()
}

func (a Uint128) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

func (a *Uint128) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if len(s) == 0 || len(s) > 39 {
		return errors.New("usage: invalid uint128 length")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return errors.New("usage: invalid uint128")
		}
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.BitLen() > 128 {
		return ErrOverflow
	}
	a.Lo = n.Uint64()
	a.Hi = n.Rsh(n, 64).Uint64()
	return nil
}

// Average returns the floor of byte-nanoseconds / covered nanoseconds. A zero
// denominator is unavailable, not zero usage. Consumers may retain the exact
// numerator and denominator for their own presentation.
func Average(area Uint128, covered uint64) (uint64, bool) {
	if covered == 0 {
		return 0, false
	}
	q, _ := area.div(covered)
	return q.Lo, q.Hi == 0
}

func add64(a, b uint64) (uint64, error) {
	n, c := bits.Add64(a, b, 0)
	if c != 0 {
		return 0, ErrOverflow
	}
	return n, nil
}
