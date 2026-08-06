package variation

import "testing"

func TestChao1WithDoubletons(t *testing.T) {
	// 5 distinct IPs: 3 singletons (f1=3), 1 doubleton (f2=1), 1 seen thrice.
	got := Chao1([]int{1, 1, 1, 2, 3})
	if got.Observed != 5 {
		t.Fatalf("observed = %d, want 5", got.Observed)
	}
	// 5 + 3^2/(2*1) = 5 + 4.5 -> floor 9 (integer estimate).
	if got.Estimate != 9 {
		t.Fatalf("estimate = %d, want 9", got.Estimate)
	}
	if got.LowerBound {
		t.Fatal("LowerBound should be false when f2 > 0")
	}
}

func TestChao1FallsBackWhenNoDoubletons(t *testing.T) {
	got := Chao1([]int{1, 1, 3})
	if got.Estimate != got.Observed || got.Observed != 3 {
		t.Fatalf("estimate = %d observed = %d, want both 3", got.Estimate, got.Observed)
	}
	if !got.LowerBound {
		t.Fatal("LowerBound should be true when f2 == 0")
	}
}

func TestChao1Empty(t *testing.T) {
	got := Chao1(nil)
	if got.Observed != 0 || got.Estimate != 0 || !got.LowerBound {
		t.Fatalf("empty estimate = %+v", got)
	}
}
