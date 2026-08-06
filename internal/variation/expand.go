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

	fixedValues, err := fixedAxisValues(fixedNames, axes)
	if err != nil {
		return nil, err
	}
	for _, name := range randomNames {
		spec := axes[name]
		if spec.Count < 1 {
			return nil, fmt.Errorf("random axis %q count must be >= 1", name)
		}
	}

	total, err := variantCount(fixedValues, randomNames, axes)
	if err != nil {
		return nil, err
	}
	if total > maxVariants {
		return nil, fmt.Errorf("%d variants exceed the limit of %d", total, maxVariants)
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
			for n := spec.From; n <= spec.To; n++ {
				nums = append(nums, strconv.Itoa(n))
			}
			values[name] = nums
		default:
			return nil, fmt.Errorf("axis %q is not a fixed axis", name)
		}
	}
	return values, nil
}

func variantCount(fixed map[string][]string, randomNames []string, axes map[string]AxisSpec) (int, error) {
	total := 1
	for _, vals := range fixed {
		total *= len(vals)
	}
	for _, name := range randomNames {
		total *= axes[name].Count
	}
	if total <= 0 {
		return 0, fmt.Errorf("axes produce no variants")
	}
	return total, nil
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

// canonicalKey serializes a cell's params as a sorted-key JSON object.
func canonicalKey(params map[string]string) (string, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([][2]string, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, [2]string{k, params[k]})
	}
	// json.Marshal of a map sorts keys already, but build explicitly to stay
	// deterministic regardless of map iteration order.
	obj := map[string]string{}
	for _, kv := range ordered {
		obj[kv[0]] = kv[1]
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
