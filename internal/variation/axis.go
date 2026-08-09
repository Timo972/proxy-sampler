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

// MaxRandomLength bounds a random axis's value length. Session identifiers are
// short; this ceiling keeps a crafted request from forcing a huge allocation.
const MaxRandomLength = 64

// DefaultRandString draws an alphanumeric value from crypto/rand. Length is
// clamped to [1, MaxRandomLength] as defense in depth; callers validate it up
// front (see plannedCount).
func DefaultRandString(length int) (string, error) {
	if length <= 0 {
		length = 8
	}
	if length > MaxRandomLength {
		length = MaxRandomLength
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
