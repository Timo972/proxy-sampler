package variation

import "testing"

func TestVariantNameWithoutParamsIsRunName(t *testing.T) {
	if got := VariantName("Frankfurt pool", nil); got != "Frankfurt pool" {
		t.Errorf("VariantName = %q, want the bare run name", got)
	}
}

func TestVariantNameSortsParamsForDeterminism(t *testing.T) {
	params := map[string]string{"region": "eu", "city": "fra", "asn": "1234"}
	want := "Pool (asn=1234,city=fra,region=eu)"
	if got := VariantName("Pool", params); got != want {
		t.Errorf("VariantName = %q, want %q", got, want)
	}
}
