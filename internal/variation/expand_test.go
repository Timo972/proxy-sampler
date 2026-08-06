package variation

import (
	"fmt"
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
