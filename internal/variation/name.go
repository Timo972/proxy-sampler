package variation

import (
	"sort"
	"strings"
)

// VariantName combines the run name with a compact, deterministic suffix
// built from the variant's sorted params so child sessions are distinguishable.
// Renaming a run recomputes this to decide which children still carry their
// generated name, so the suffix must stay stable for a given param set.
func VariantName(runName string, params map[string]string) string {
	if len(params) == 0 {
		return runName
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return runName + " (" + strings.Join(parts, ",") + ")"
}
