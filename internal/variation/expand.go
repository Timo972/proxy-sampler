package variation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Variant is one fully-resolved proxy URL plus the params that produced it.
type Variant struct {
	Params  map[string]string
	CellKey string
	URL     string
}

const defaultRandomLength = 8

// Expand renders template × axes into variants. List and range axes form the
// cartesian product of cells; each random axis multiplies every cell by its
// fresh values. Every placeholder must have an axis and vice versa.
func Expand(t Template, axes map[string]AxisSpec, rnd RandString, maxVariants int) ([]Variant, error) {
	placeholders := t.Placeholders()
	placeholderSet := map[string]struct{}{}
	for _, name := range placeholders {
		placeholderSet[name] = struct{}{}
		if _, ok := axes[name]; !ok {
			return nil, fmt.Errorf("placeholder %q has no axis", name)
		}
	}
	for name := range axes {
		if _, ok := placeholderSet[name]; !ok {
			return nil, fmt.Errorf("axis %q has no matching placeholder", name)
		}
	}

	// Split axes into deterministic (list/range) and random, in sorted order.
	var fixedNames, randomNames []string
	for _, name := range placeholders {
		if axes[name].Kind == AxisRandom {
			randomNames = append(randomNames, name)
		} else {
			fixedNames = append(fixedNames, name)
		}
	}
	sort.Strings(fixedNames)
	sort.Strings(randomNames)

	// Check variant count and cap before materializing any axis values. The
	// cap is enforced incrementally so a crafted set of axes cannot overflow
	// the product past the limit.
	total, err := plannedCount(placeholders, axes, maxVariants)
	if err != nil {
		return nil, err
	}

	fixedValues, err := fixedAxisValues(fixedNames, axes)
	if err != nil {
		return nil, err
	}

	cells := cartesian(fixedNames, fixedValues)
	variants := make([]Variant, 0, total)
	for _, cell := range cells {
		cellKey, err := canonicalKey(cell)
		if err != nil {
			return nil, err
		}
		randomCombos, err := randomCartesian(randomNames, axes, rnd)
		if err != nil {
			return nil, err
		}
		for _, combo := range randomCombos {
			params := map[string]string{}
			for k, v := range cell {
				params[k] = v
			}
			for k, v := range combo {
				params[k] = v
			}
			url, err := t.Render(params)
			if err != nil {
				return nil, err
			}
			variants = append(variants, Variant{Params: params, CellKey: cellKey, URL: url})
		}
	}
	return variants, nil
}

// plannedCount computes the total number of variants without materializing
// any axis values. This allows us to validate and check the cap before
// allocating large slices for range axes.
func plannedCount(placeholders []string, axes map[string]AxisSpec, maxVariants int) (int, error) {
	total := 1
	for _, name := range placeholders {
		spec := axes[name]
		var size int
		switch spec.Kind {
		case AxisList:
			if len(spec.Values) == 0 {
				return 0, fmt.Errorf("list axis %q has no values", name)
			}
			size = len(spec.Values)
		case AxisRange:
			if spec.From > spec.To {
				return 0, fmt.Errorf("range axis %q has from > to", name)
			}
			// Guard the width computation against overflow (e.g. To==MaxInt)
			// before it participates in the running product.
			if spec.To-spec.From < 0 || spec.To-spec.From >= maxVariants {
				return 0, fmt.Errorf("range axis %q exceeds the limit of %d", name, maxVariants)
			}
			size = spec.To - spec.From + 1
		case AxisRandom:
			if spec.Count < 1 {
				return 0, fmt.Errorf("random axis %q count must be >= 1", name)
			}
			if spec.Length < 0 || spec.Length > MaxRandomLength {
				return 0, fmt.Errorf("random axis %q length must be between 0 and %d", name, MaxRandomLength)
			}
			size = spec.Count
		default:
			return 0, fmt.Errorf("axis %q has unknown kind %q", name, spec.Kind)
		}
		// Enforce the cap on every multiplication so the product can never
		// overflow past maxVariants (division avoids computing total*size).
		if size > maxVariants || total > maxVariants/size {
			return 0, fmt.Errorf("variants exceed the limit of %d", maxVariants)
		}
		total *= size
	}
	if total <= 0 {
		return 0, fmt.Errorf("axes produce no variants")
	}
	return total, nil
}

func fixedAxisValues(names []string, axes map[string]AxisSpec) (map[string][]string, error) {
	values := map[string][]string{}
	for _, name := range names {
		spec := axes[name]
		switch spec.Kind {
		case AxisList:
			if len(spec.Values) == 0 {
				return nil, fmt.Errorf("list axis %q has no values", name)
			}
			values[name] = spec.Values
		case AxisRange:
			if spec.From > spec.To {
				return nil, fmt.Errorf("range axis %q has from > to", name)
			}
			nums := make([]string, 0, spec.To-spec.From+1)
			// Break after appending To rather than incrementing past it, so a
			// range ending at the maximum integer cannot wrap and loop forever.
			for n := spec.From; ; n++ {
				nums = append(nums, strconv.Itoa(n))
				if n == spec.To {
					break
				}
			}
			values[name] = nums
		default:
			return nil, fmt.Errorf("axis %q is not a fixed axis", name)
		}
	}
	return values, nil
}

func cartesian(names []string, values map[string][]string) []map[string]string {
	combos := []map[string]string{{}}
	for _, name := range names {
		var next []map[string]string
		for _, base := range combos {
			for _, value := range values[name] {
				merged := map[string]string{}
				for k, v := range base {
					merged[k] = v
				}
				merged[name] = value
				next = append(next, merged)
			}
		}
		combos = next
	}
	return combos
}

func randomCartesian(names []string, axes map[string]AxisSpec, rnd RandString) ([]map[string]string, error) {
	combos := []map[string]string{{}}
	for _, name := range names {
		spec := axes[name]
		length := spec.Length
		if length <= 0 {
			length = defaultRandomLength
		}
		var next []map[string]string
		for _, base := range combos {
			for i := 0; i < spec.Count; i++ {
				value, err := rnd(length)
				if err != nil {
					return nil, err
				}
				merged := map[string]string{}
				for k, v := range base {
					merged[k] = v
				}
				merged[name] = value
				next = append(next, merged)
			}
		}
		combos = next
	}
	return combos, nil
}

// canonicalKey serializes a cell's params as a JSON object with sorted keys
// (encoding/json marshals map keys in sorted order deterministically).
func canonicalKey(params map[string]string) (string, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
