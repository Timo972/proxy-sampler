package variation

// Estimate is a Chao1 pool-size estimate. LowerBound marks the fallback where
// the estimator is undefined (no doubletons) and Estimate equals Observed.
type Estimate struct {
	Observed   int
	Estimate   int
	LowerBound bool
}

// Chao1 estimates the true pool size from per-IP variant frequencies.
// Each element is how many variants observed that distinct IP.
func Chao1(variantFrequencies []int) Estimate {
	observed := len(variantFrequencies)
	f1, f2 := 0, 0
	for _, freq := range variantFrequencies {
		switch freq {
		case 1:
			f1++
		case 2:
			f2++
		}
	}
	if observed == 0 || f2 == 0 {
		return Estimate{Observed: observed, Estimate: observed, LowerBound: true}
	}
	estimate := observed + (f1*f1)/(2*f2)
	return Estimate{Observed: observed, Estimate: estimate, LowerBound: false}
}
