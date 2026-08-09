package variation

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// stubRand yields deterministic values: r0, r1, r2... per call.
func stubRand() RandString {
	n := 0
	return func(int) (string, error) {
		v := fmt.Sprintf("r%d", n)
		n++
		return v, nil
	}
}

func TestExpandCartesianTimesRandom(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{country}-{session}@gate:1080")
	axes := map[string]AxisSpec{
		"country": {Kind: AxisList, Values: []string{"de", "us"}},
		"session": {Kind: AxisRandom, Count: 3, Length: 4},
	}
	variants, err := Expand(tmpl, axes, stubRand(), 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 6 {
		t.Fatalf("variants = %d, want 6", len(variants))
	}
	// Each cell key covers the non-random params only.
	cells := map[string]int{}
	for _, v := range variants {
		cells[v.CellKey]++
	}
	if len(cells) != 2 {
		t.Fatalf("cells = %d, want 2", len(cells))
	}
	for key, count := range cells {
		if count != 3 {
			t.Fatalf("cell %s has %d variants, want 3", key, count)
		}
	}
}

func TestExpandRangeInclusive(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u:pw@gate:{port}")
	axes := map[string]AxisSpec{"port": {Kind: AxisRange, From: 10000, To: 10002}}
	variants, err := Expand(tmpl, axes, stubRand(), 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 3 {
		t.Fatalf("variants = %d, want 3", len(variants))
	}
	if !strings.HasSuffix(variants[0].URL, ":10000") {
		t.Fatalf("first url = %q", variants[0].URL)
	}
}

func TestExpandRejectsUnusedAxis(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u:pw@gate:1080")
	axes := map[string]AxisSpec{"country": {Kind: AxisList, Values: []string{"de"}}}
	if _, err := Expand(tmpl, axes, stubRand(), 128); err == nil {
		t.Fatal("expected error: axis without placeholder")
	}
}

func TestExpandRejectsMissingAxis(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{country}@gate:1080")
	if _, err := Expand(tmpl, map[string]AxisSpec{}, stubRand(), 128); err == nil {
		t.Fatal("expected error: placeholder without axis")
	}
}

func TestExpandRejectsBadRange(t *testing.T) {
	tmpl, _ := ParseTemplate("p://gate:{port}")
	axes := map[string]AxisSpec{"port": {Kind: AxisRange, From: 5, To: 1}}
	if _, err := Expand(tmpl, axes, stubRand(), 128); err == nil {
		t.Fatal("expected error: from > to")
	}
}

func TestExpandRejectsRandomCountBelowOne(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{s}@gate:1080")
	axes := map[string]AxisSpec{"s": {Kind: AxisRandom, Count: 0, Length: 4}}
	if _, err := Expand(tmpl, axes, stubRand(), 128); err == nil {
		t.Fatal("expected error: count < 1")
	}
}

func TestExpandEnforcesCap(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{a}-{b}@gate:1080")
	axes := map[string]AxisSpec{
		"a": {Kind: AxisRange, From: 1, To: 10},
		"b": {Kind: AxisRange, From: 1, To: 10},
	}
	if _, err := Expand(tmpl, axes, stubRand(), 50); err == nil {
		t.Fatal("expected error: 100 variants exceeds cap 50")
	}
}

func TestExpandRandomValuesAreDistinctWithinCell(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{s}@gate:1080")
	// crypto/rand-backed generator; count near the length-2 space still yields
	// distinct values per cell.
	axes := map[string]AxisSpec{"s": {Kind: AxisRandom, Count: 30, Length: 2}}
	variants, err := Expand(tmpl, axes, DefaultRandString, 128)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct{}{}
	for _, v := range variants {
		if _, dup := seen[v.URL]; dup {
			t.Fatalf("duplicate variant URL: %q", v.URL)
		}
		seen[v.URL] = struct{}{}
	}
	if len(seen) != 30 {
		t.Fatalf("distinct variants = %d, want 30", len(seen))
	}
}

func TestExpandRejectsRandomCountExceedingValueSpace(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{s}@gate:1080")
	// length 1 over a 36-char alphabet has only 36 distinct values.
	axes := map[string]AxisSpec{"s": {Kind: AxisRandom, Count: 100, Length: 1}}
	if _, err := Expand(tmpl, axes, DefaultRandString, 128); err == nil {
		t.Fatal("expected error: count exceeds the length-1 value space")
	}
}

func TestExpandRejectsHugeRandomLength(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{s}@gate:1080")
	axes := map[string]AxisSpec{"s": {Kind: AxisRandom, Count: 1, Length: MaxRandomLength + 1}}
	if _, err := Expand(tmpl, axes, stubRand(), 128); err == nil {
		t.Fatal("expected error: random length exceeds MaxRandomLength")
	}
}

func TestExpandRangeAtMaxIntDoesNotWrap(t *testing.T) {
	tmpl, _ := ParseTemplate("p://gate:{port}")
	// A single-value range at the maximum int must produce exactly one value
	// and terminate, not loop forever via integer wraparound.
	axes := map[string]AxisSpec{"port": {Kind: AxisRange, From: math.MaxInt, To: math.MaxInt}}
	variants, err := Expand(tmpl, axes, stubRand(), 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(variants))
	}
}

func TestExpandProductOverflowCannotBypassCap(t *testing.T) {
	tmpl, _ := ParseTemplate("p://u-{a}-{b}-{c}@gate:1080")
	// Axis sizes whose product overflows int back to a small positive value
	// must still be rejected by the cap, not allowed to attempt a huge range
	// allocation.
	axes := map[string]AxisSpec{
		"a": {Kind: AxisRange, From: 0, To: math.MaxInt},
		"b": {Kind: AxisList, Values: []string{"x", "y"}},
		"c": {Kind: AxisRange, From: 0, To: math.MaxInt},
	}
	if _, err := Expand(tmpl, axes, stubRand(), 128); err == nil {
		t.Fatal("expected error: overflowing product must not bypass the cap")
	}
}
