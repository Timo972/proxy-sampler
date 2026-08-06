package variation

import (
	"crypto/rand"
	"math/big"
)

// AxisKind enumerates the ways an axis produces values.
type AxisKind string

const (
	AxisList   AxisKind = "list"
	AxisRange  AxisKind = "range"
	AxisRandom AxisKind = "random"
)

// AxisSpec describes one parameter axis. Only the fields for its Kind apply.
type AxisSpec struct {
	Kind   AxisKind
	Values []string // list
	From   int      // range (inclusive)
	To     int      // range (inclusive)
	Count  int      // random: values per cell
	Length int      // random: value length
}

// RandString generates one random value of the given length.
type RandString func(length int) (string, error)

const randAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// DefaultRandString draws an alphanumeric value from crypto/rand.
func DefaultRandString(length int) (string, error) {
	if length <= 0 {
		length = 8
	}
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(randAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = randAlphabet[n.Int64()]
	}
	return string(out), nil
}
