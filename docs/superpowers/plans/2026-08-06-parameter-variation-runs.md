# Parameter Variation Runs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expand a proxy-URL template across parameter axes into many child sampling sessions, then aggregate their exit IPs into a pool-level, IP-deduped statistical report.

**Architecture:** A `variation_runs` Postgres row owns K ordinary `sampling_sessions` (linked by a nullable `run_id`). Run creation expands `template × axes` into K encrypted proxy URLs and creates+starts each as a normal session through the existing supervisor/worker/report machinery — nothing downstream changes. A run's report is a server-side aggregation over its children's `session_ips` (deduped by IP) plus a ClickHouse time series over `session_id IN (...)`. A new pure `internal/variation` package holds template parsing, axis expansion, the Chao1 pool-size estimator, and the report builder.

**Tech Stack:** Go 1.x (pgx/v5, sqlc, chi, oapi-codegen, ClickHouse), React + TypeScript + Vite + TanStack Query + react-router, goose migrations.

## Global Constraints

- Module path: `github.com/timo972/proxy-sampler` (all imports).
- Go domain packages (`internal/session`, `internal/variation`) hold **no** DB or HTTP imports — interfaces and pure functions only; implementations live in `internal/db`, `internal/ch`, `internal/api`.
- Follow existing naming conventions: noun-style methods, no `GetX` getters; use `Fetch`/`Save`/`Acquire` verbs where a verb is needed.
- Regenerate code, never hand-edit generated files: `make sqlc-generate` after `.sql` changes, `make openapi-generate` after `api/openapi.yaml` changes. `make generate-check` must pass (git diff clean).
- Persisted integers are capped at `2147483647` (int32) — mirror the existing `maxPersistedInteger` guard.
- Proxy templates are secrets: store encrypted via `crypto.Cipher.Encrypt`, never return plaintext or password in any API response. `template_display` is credential-free.
- Config default: `MAX_VARIANTS_PER_RUN` = 128.
- Every task ends green: `make go-test` (Go) or `cd web && npm test -- --run` (web). Integration tests (`internal/db`, `internal/ch`) need Postgres/ClickHouse via `make integration-test` and may be skipped locally if the datastores are absent, following the existing `testStore`/reader skip pattern.
- Commit after each task with a `feat:`/`test:`/`chore:` message.

---

### Task 1: Config — MaxVariantsPerRun

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.MaxVariantsPerRun int` (default 128), parsed from `MAX_VARIANTS_PER_RUN`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoadDefaultsMaxVariantsPerRun(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"DATABASE_URL":   "postgres://x",
		"CLICKHOUSE_DSN": "clickhouse://x",
		"ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxVariantsPerRun != 128 {
		t.Fatalf("MaxVariantsPerRun = %d, want 128", cfg.MaxVariantsPerRun)
	}
}

func TestLoadOverridesMaxVariantsPerRun(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"DATABASE_URL":         "postgres://x",
		"CLICKHOUSE_DSN":       "clickhouse://x",
		"ENCRYPTION_KEY":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"MAX_VARIANTS_PER_RUN": "20",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxVariantsPerRun != 20 {
		t.Fatalf("MaxVariantsPerRun = %d, want 20", cfg.MaxVariantsPerRun)
	}
}

func TestLoadRejectsNonPositiveMaxVariantsPerRun(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"DATABASE_URL":         "postgres://x",
		"CLICKHOUSE_DSN":       "clickhouse://x",
		"ENCRYPTION_KEY":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"MAX_VARIANTS_PER_RUN": "0",
	}))
	if err == nil {
		t.Fatal("expected error for non-positive MAX_VARIANTS_PER_RUN")
	}
}
```

If `envFunc`/`base64` helpers are not already present in the test file, add:

```go
func envFunc(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
```

and ensure `import "encoding/base64"` is present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run MaxVariantsPerRun -v`
Expected: FAIL (`cfg.MaxVariantsPerRun` undefined).

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the field to `Config`:

```go
	DBAutoMigrate      bool
	MaxVariantsPerRun  int
```

In `Load`, set the default in the struct literal:

```go
		DBAutoMigrate:      true,
		MaxVariantsPerRun:  128,
```

After the `DB_AUTO_MIGRATE` block, before `if err != nil {`:

```go
	if err == nil {
		if raw := getenv("MAX_VARIANTS_PER_RUN"); raw != "" {
			cfg.MaxVariantsPerRun, err = positiveInt(raw, "MAX_VARIANTS_PER_RUN")
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add MAX_VARIANTS_PER_RUN config"
```

---

### Task 2: variation package — template parsing and rendering

**Files:**
- Create: `internal/variation/template.go`
- Test: `internal/variation/template_test.go`

**Interfaces:**
- Produces:
  - `type Template struct { ... }` (opaque; fields unexported)
  - `func ParseTemplate(raw string) (Template, error)` — extracts `{name}` placeholders; error on empty raw, empty `{}`, or duplicate placeholder names allowed (dedup silently).
  - `func (t Template) Placeholders() []string` — sorted, de-duplicated placeholder names.
  - `func (t Template) Render(values map[string]string) (string, error)` — substitutes every `{name}`; errors if a placeholder has no value in `values`.

- [ ] **Step 1: Write the failing test**

```go
package variation

import (
	"reflect"
	"testing"
)

func TestParseTemplatePlaceholders(t *testing.T) {
	tmpl, err := ParseTemplate("socks5h://u-cc-{country}-sid-{session}:pw@gate:1080")
	if err != nil {
		t.Fatal(err)
	}
	if got := tmpl.Placeholders(); !reflect.DeepEqual(got, []string{"country", "session"}) {
		t.Fatalf("placeholders = %v", got)
	}
}

func TestParseTemplateRejectsEmptyPlaceholder(t *testing.T) {
	if _, err := ParseTemplate("socks5h://u-{}:pw@gate:1080"); err == nil {
		t.Fatal("expected error for empty placeholder")
	}
}

func TestParseTemplateRejectsBlank(t *testing.T) {
	if _, err := ParseTemplate("   "); err == nil {
		t.Fatal("expected error for blank template")
	}
}

func TestRenderSubstitutesValues(t *testing.T) {
	tmpl, _ := ParseTemplate("host:{port}/u-{country}")
	got, err := tmpl.Render(map[string]string{"port": "10001", "country": "de"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "host:10001/u-de" {
		t.Fatalf("render = %q", got)
	}
}

func TestRenderMissingValueErrors(t *testing.T) {
	tmpl, _ := ParseTemplate("host:{port}")
	if _, err := tmpl.Render(map[string]string{}); err == nil {
		t.Fatal("expected error for missing value")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/variation/ -run Template -v`
Expected: FAIL (`ParseTemplate` undefined).

- [ ] **Step 3: Implement**

```go
// Package variation expands proxy-URL templates across parameter axes and
// aggregates the resulting exit-IP observations into pool statistics.
package variation

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var placeholderPattern = regexp.MustCompile(`\{([^{}]*)\}`)

// Template is a proxy URL with {name} placeholders substituted at expansion.
type Template struct {
	raw   string
	names []string
}

// ParseTemplate extracts the placeholder names from a proxy-URL template.
func ParseTemplate(raw string) (Template, error) {
	if strings.TrimSpace(raw) == "" {
		return Template{}, errors.New("template is empty")
	}
	matches := placeholderPattern.FindAllStringSubmatch(raw, -1)
	seen := map[string]struct{}{}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		name := strings.TrimSpace(match[1])
		if name == "" {
			return Template{}, errors.New("template has an empty {} placeholder")
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return Template{raw: raw, names: names}, nil
}

// Placeholders returns the sorted, de-duplicated placeholder names.
func (t Template) Placeholders() []string {
	out := make([]string, len(t.names))
	copy(out, t.names)
	return out
}

// Render substitutes every placeholder with its value.
func (t Template) Render(values map[string]string) (string, error) {
	var missing error
	out := placeholderPattern.ReplaceAllStringFunc(t.raw, func(token string) string {
		name := strings.TrimSpace(token[1 : len(token)-1])
		value, ok := values[name]
		if !ok {
			missing = fmt.Errorf("no value for placeholder %q", name)
			return token
		}
		return value
	})
	if missing != nil {
		return "", missing
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/variation/ -run Template -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/variation/template.go internal/variation/template_test.go
git commit -m "feat: add variation template parsing and rendering"
```

---

### Task 3: variation package — axes and expansion

**Files:**
- Create: `internal/variation/axis.go`
- Create: `internal/variation/expand.go`
- Test: `internal/variation/expand_test.go`

**Interfaces:**
- Consumes: `Template`, `Template.Placeholders`, `Template.Render` (Task 2).
- Produces:
  - `type AxisKind string` with `AxisList = "list"`, `AxisRange = "range"`, `AxisRandom = "random"`.
  - `type AxisSpec struct { Kind AxisKind; Values []string; From, To int; Count, Length int }`.
  - `type Variant struct { Params map[string]string; CellKey string; URL string }`.
  - `type RandString func(length int) (string, error)` — injectable value generator for random axes.
  - `func Expand(t Template, axes map[string]AxisSpec, rnd RandString, maxVariants int) ([]Variant, error)`.
  - `func DefaultRandString(length int) (string, error)` — crypto/rand alphanumeric.
  - `CellKey` = canonical JSON object of the non-random params (sorted keys).

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/variation/ -run Expand -v`
Expected: FAIL (`Expand` undefined).

- [ ] **Step 3: Implement `axis.go`**

```go
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
```

- [ ] **Step 4: Implement `expand.go`**

```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/variation/ -v`
Expected: PASS (all template + expand tests).

- [ ] **Step 6: Commit**

```bash
git add internal/variation/axis.go internal/variation/expand.go internal/variation/expand_test.go
git commit -m "feat: add variation axis expansion"
```

---

### Task 4: variation package — Chao1 pool-size estimator

**Files:**
- Create: `internal/variation/chao1.go`
- Test: `internal/variation/chao1_test.go`

**Interfaces:**
- Produces:
  - `type Estimate struct { Observed, Estimate int; LowerBound bool }`.
  - `func Chao1(variantFrequencies []int) Estimate` — input: one element per distinct IP giving the number of variants that observed it. `Ŝ = S_obs + f1²/(2·f2)`; when `f2 == 0`, `Estimate = Observed` and `LowerBound = true`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/variation/ -run Chao1 -v`
Expected: FAIL (`Chao1` undefined).

- [ ] **Step 3: Implement**

```go
package variation

// Estimate is a Chao1 pool-size estimate. LowerBound marks the fallback where
// the estimator is undefined (no doubletons) and Estimate equals Observed.
type Estimate struct {
	Observed  int
	Estimate  int
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/variation/ -run Chao1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/variation/chao1.go internal/variation/chao1_test.go
git commit -m "feat: add Chao1 pool-size estimator"
```

---

### Task 5: variation package — run domain types and pool report builder

**Files:**
- Create: `internal/variation/run.go` (domain types + Store interface)
- Create: `internal/variation/report.go` (pure aggregation)
- Test: `internal/variation/report_test.go`

**Interfaces:**
- Consumes: `Chao1` (Task 4), `session.Reputation`, `session.Snapshot`, `session.Status` (existing).
- Produces:
  - `type Run struct { ID uuid.UUID; Name string; TemplateCiphertext, TemplateNonce []byte; TemplateDisplay string; Axes json.RawMessage; CreatedAt time.Time }`.
  - `type ChildSession struct { Session session.Session; Params json.RawMessage; CellKey string }`.
  - `type RunStatus string` (`RunRunning`/`RunStopped`/`RunFinished`).
  - `type RunSummary struct { Run; VariantCount int; DistinctIPs int; Status RunStatus }`.
  - `type VariantSession struct { SessionID uuid.UUID; Name string; Params json.RawMessage; CellKey string; Status session.Status; Snapshot session.Snapshot }`.
  - `type IPObservation struct { SessionID uuid.UUID; IP netip.Addr; HitCount int64; Reputation *session.Reputation }`.
  - `type Store interface { CreateRun(context.Context, Run, []ChildSession) error; Runs(context.Context) ([]RunSummary, error); RunByID(context.Context, uuid.UUID) (RunSummary, error); RunSessions(context.Context, uuid.UUID) ([]VariantSession, error); RunIPObservations(context.Context, uuid.UUID) ([]IPObservation, error); DeleteRun(context.Context, uuid.UUID) error }`.
  - `var ErrRunNotFound = errors.New("variation run not found")`.
  - Report types: `Composition`, `RiskBucket`, `IPRow`, `CellReport`, `PoolReport`.
  - `func BuildPoolReport(variants []VariantSession, observations []IPObservation) PoolReport`.

- [ ] **Step 1: Write `run.go`**

```go
package variation

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

// ErrRunNotFound is returned when a run row does not exist.
var ErrRunNotFound = errors.New("variation run not found")

// RunStatus is a run's status, derived from its children.
type RunStatus string

const (
	RunRunning  RunStatus = "running"
	RunStopped  RunStatus = "stopped"
	RunFinished RunStatus = "finished"
)

// Run is a durable variation run owning many child sessions.
type Run struct {
	ID                 uuid.UUID
	Name               string
	TemplateCiphertext []byte
	TemplateNonce      []byte
	TemplateDisplay    string
	Axes               json.RawMessage
	CreatedAt          time.Time
}

// ChildSession pairs a session with its resolved variant params for insertion.
type ChildSession struct {
	Session session.Session
	Params  json.RawMessage
	CellKey string
}

// RunSummary is a run plus its rolled-up counts and derived status.
type RunSummary struct {
	Run
	VariantCount int
	DistinctIPs  int
	Status       RunStatus
}

// VariantSession is one child session's summary within a run.
type VariantSession struct {
	SessionID uuid.UUID
	Name      string
	Params    json.RawMessage
	CellKey   string
	Status    session.Status
	Snapshot  session.Snapshot
}

// IPObservation is one child session observing one exit IP, with reputation.
type IPObservation struct {
	SessionID  uuid.UUID
	IP         netip.Addr
	HitCount   int64
	FirstSeen  time.Time
	LastSeen   time.Time
	Reputation *session.Reputation
}

// Store persists and reads variation runs and their aggregates.
type Store interface {
	CreateRun(ctx context.Context, run Run, children []ChildSession) error
	Runs(ctx context.Context) ([]RunSummary, error)
	RunByID(ctx context.Context, id uuid.UUID) (RunSummary, error)
	RunSessions(ctx context.Context, id uuid.UUID) ([]VariantSession, error)
	RunIPObservations(ctx context.Context, id uuid.UUID) ([]IPObservation, error)
	DeleteRun(ctx context.Context, id uuid.UUID) error
}
```

- [ ] **Step 2: Write the failing report test**

```go
package variation

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

func rep(country, isp, category string) *session.Reputation {
	return &session.Reputation{Country: country, ISP: isp, Category: category}
}

func TestBuildPoolReportDedupesAndCounts(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	ipA := netip.MustParseAddr("1.1.1.1")
	ipB := netip.MustParseAddr("2.2.2.2")
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
		{SessionID: s2, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
	}
	obs := []IPObservation{
		{SessionID: s1, IP: ipA, HitCount: 4, Reputation: rep("DE", "ISP-1", "residential")},
		{SessionID: s2, IP: ipA, HitCount: 2, Reputation: rep("DE", "ISP-1", "residential")}, // shared IP
		{SessionID: s2, IP: ipB, HitCount: 1, Reputation: rep("DE", "ISP-1", "residential")},
	}
	report := BuildPoolReport(variants, obs)
	if report.DistinctIPs != 2 {
		t.Fatalf("distinct ips = %d, want 2 (deduped)", report.DistinctIPs)
	}
	if report.Composition.Residential != 2 {
		t.Fatalf("residential = %d, want 2", report.Composition.Residential)
	}
	if len(report.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(report.Cells))
	}
	// Both variants requested country=de, dominant observed DE -> honored.
	if report.HonorRate == nil || *report.HonorRate != 1 {
		t.Fatalf("honor rate = %v, want 1", report.HonorRate)
	}
}

func TestBuildPoolReportHonorMismatch(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"jp"}`, Params: json.RawMessage(`{"country":"jp"}`), Snapshot: session.Snapshot{SamplesTaken: 2}},
	}
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("3.3.3.3"), HitCount: 5, Reputation: rep("US", "ISP-2", "datacenter")},
	}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate == nil || *report.HonorRate != 0 {
		t.Fatalf("honor rate = %v, want 0 (requested jp, observed US)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorUnknownWithoutSamples(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 0}},
	}
	report := BuildPoolReport(variants, nil)
	if report.HonorRate != nil {
		t.Fatalf("honor rate = %v, want nil (no samples)", report.HonorRate)
	}
	if report.EstimatedPoolSize != 0 {
		t.Fatalf("pool size = %d, want 0", report.EstimatedPoolSize)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/variation/ -run PoolReport -v`
Expected: FAIL (`BuildPoolReport` undefined).

- [ ] **Step 4: Implement `report.go`**

```go
package variation

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

// Composition counts distinct IPs by network category.
type Composition struct {
	Mobile      int
	Residential int
	Datacenter  int
	Unknown     int
}

// RiskBucket is one 10-wide risk-score histogram bin.
type RiskBucket struct {
	Label string
	Min   int
	Max   int
	Count int
}

// IPRow is one deduped pool IP with its reputation for display/export.
type IPRow struct {
	IP             string
	Category       string
	Country        string
	ISP            string
	ASN            string
	RiskScore      *int
	GreyNoiseClass string
	DNSBLListed    bool
	DNSBLHits      []string
	HitCount       int64
	FirstSeen      time.Time
	LastSeen       time.Time
}

// CellReport summarizes one parameter cell (params minus random axes).
type CellReport struct {
	CellKey      string
	Params       json.RawMessage
	VariantCount int
	DistinctIPs  int
	HonorRate    *float64
	Composition  Composition
	MedianRisk   *int
	SuccessRate  float64
}

// PoolReport is the aggregated, IP-deduped view over a whole run.
type PoolReport struct {
	DistinctIPs        int
	EstimatedPoolSize  int
	PoolSizeLowerBound bool
	Composition        Composition
	RiskHistogram      []RiskBucket
	FlaggedIPs         int
	FlaggedPercent     float64
	DNSBLHitIPs        int
	HonorRate          *float64
	Cells              []CellReport
	IPs                []IPRow
}

// BuildPoolReport aggregates variant sessions and their IP observations into a
// pool-level report, deduping every statistic by distinct exit IP.
func BuildPoolReport(variants []VariantSession, observations []IPObservation) PoolReport {
	report := PoolReport{RiskHistogram: newRiskHistogram(), IPs: []IPRow{}, Cells: []CellReport{}}

	// Distinct IP -> merged row + set of variant sessions that saw it.
	ips := map[netip.Addr]*ipAccum{}
	// Per-session observed rollups for honor-rate computation.
	sessionObs := map[uuid.UUID][]IPObservation{}
	for _, obs := range observations {
		sessionObs[obs.SessionID] = append(sessionObs[obs.SessionID], obs)
		accum, ok := ips[obs.IP]
		if !ok {
			accum = &ipAccum{row: newIPRow(obs), variants: map[uuid.UUID]struct{}{}}
			ips[obs.IP] = accum
		} else {
			accum.row.HitCount += obs.HitCount
			if !obs.FirstSeen.IsZero() && (accum.row.FirstSeen.IsZero() || obs.FirstSeen.Before(accum.row.FirstSeen)) {
				accum.row.FirstSeen = obs.FirstSeen
			}
			if obs.LastSeen.After(accum.row.LastSeen) {
				accum.row.LastSeen = obs.LastSeen
			}
		}
		accum.variants[obs.SessionID] = struct{}{}
	}

	frequencies := make([]int, 0, len(ips))
	for _, accum := range sortedIPs(ips) {
		frequencies = append(frequencies, len(accum.variants))
		applyComposition(&report.Composition, accum.row.Category)
		if flagged := isFlagged(accum.row); flagged {
			report.FlaggedIPs++
		}
		if accum.row.DNSBLListed {
			report.DNSBLHitIPs++
		}
		if accum.row.RiskScore != nil {
			addRisk(report.RiskHistogram, *accum.row.RiskScore)
		}
		report.IPs = append(report.IPs, accum.row)
	}
	report.DistinctIPs = len(ips)
	if report.DistinctIPs > 0 {
		report.FlaggedPercent = float64(report.FlaggedIPs) * 100 / float64(report.DistinctIPs)
	}
	estimate := Chao1(frequencies)
	report.EstimatedPoolSize = estimate.Estimate
	report.PoolSizeLowerBound = estimate.LowerBound

	report.HonorRate, report.Cells = buildHonorAndCells(variants, sessionObs)
	return report
}

func newIPRow(obs IPObservation) IPRow {
	row := IPRow{
		IP: obs.IP.String(), Category: "unknown", DNSBLHits: []string{},
		HitCount: obs.HitCount, FirstSeen: obs.FirstSeen, LastSeen: obs.LastSeen,
	}
	if r := obs.Reputation; r != nil {
		row.Category = normalizedCategory(r.Category)
		row.Country, row.ISP, row.ASN = r.Country, r.ISP, r.ASN
		row.GreyNoiseClass = r.GreyNoiseClass
		if r.RiskScore != nil {
			score := *r.RiskScore
			row.RiskScore = &score
		}
		if r.DNSBLListed != nil {
			row.DNSBLListed = *r.DNSBLListed
		}
		row.DNSBLHits = append(row.DNSBLHits, r.DNSBLHits...)
	}
	return row
}

func isFlagged(row IPRow) bool {
	return (row.RiskScore != nil && *row.RiskScore >= 70) ||
		strings.EqualFold(row.GreyNoiseClass, "malicious") || row.DNSBLListed
}

func buildHonorAndCells(variants []VariantSession, sessionObs map[uuid.UUID][]IPObservation) (*float64, []CellReport) {
	type cellAccum struct {
		key          string
		params       json.RawMessage
		variantCount int
		honored      int
		sampled      int
		ips          map[netip.Addr]struct{}
		composition  Composition
	}
	order := []string{}
	cells := map[string]*cellAccum{}
	globalHonored, globalSampled := 0, 0

	for _, v := range variants {
		accum, ok := cells[v.CellKey]
		if !ok {
			accum = &cellAccum{key: v.CellKey, params: v.Params, ips: map[netip.Addr]struct{}{}}
			cells[v.CellKey] = accum
			order = append(order, v.CellKey)
		}
		accum.variantCount++
		obs := sessionObs[v.SessionID]
		for _, o := range obs {
			if _, seen := accum.ips[o.IP]; !seen {
				accum.ips[o.IP] = struct{}{}
				applyComposition(&accum.composition, normalizedCategory(reputationCategory(o.Reputation)))
			}
		}
		if v.Snapshot.SamplesTaken == 0 || len(obs) == 0 {
			continue
		}
		accum.sampled++
		globalSampled++
		if honored := variantHonored(v.Params, obs); honored {
			accum.honored++
			globalHonored++
		}
	}

	result := make([]CellReport, 0, len(order))
	for _, key := range order {
		accum := cells[key]
		cell := CellReport{
			CellKey: accum.key, Params: accum.params, VariantCount: accum.variantCount,
			DistinctIPs: len(accum.ips), Composition: accum.composition,
		}
		if accum.sampled > 0 {
			rate := float64(accum.honored) / float64(accum.sampled)
			cell.HonorRate = &rate
		}
		result = append(result, cell)
	}
	var honorRate *float64
	if globalSampled > 0 {
		rate := float64(globalHonored) / float64(globalSampled)
		honorRate = &rate
	}
	return honorRate, result
}

// variantHonored compares requested country/isp params against the dominant
// observed values (weighted by hit count) across the variant's IPs.
func variantHonored(params json.RawMessage, obs []IPObservation) bool {
	requested := map[string]string{}
	_ = json.Unmarshal(params, &requested)
	country := firstParam(requested, "country", "cc")
	isp := firstParam(requested, "isp")
	if country == "" && isp == "" {
		return true // nothing targeted -> nothing to violate
	}
	if country != "" && !strings.EqualFold(dominant(obs, func(r *session.Reputation) string { return r.Country }), country) {
		return false
	}
	if isp != "" && !strings.EqualFold(dominant(obs, func(r *session.Reputation) string { return r.ISP }), isp) {
		return false
	}
	return true
}

func dominant(obs []IPObservation, pick func(*session.Reputation) string) string {
	weights := map[string]int64{}
	for _, o := range obs {
		if o.Reputation == nil {
			continue
		}
		value := pick(o.Reputation)
		if value == "" {
			continue
		}
		weights[value] += o.HitCount + 1
	}
	best, bestWeight := "", int64(0)
	for value, weight := range weights {
		if weight > bestWeight {
			best, bestWeight = value, weight
		}
	}
	return best
}

func firstParam(params map[string]string, keys ...string) string {
	for _, key := range keys {
		if v, ok := params[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

func reputationCategory(r *session.Reputation) string {
	if r == nil {
		return "unknown"
	}
	return r.Category
}

// ipAccum merges every observation of one distinct exit IP across variants.
type ipAccum struct {
	row      IPRow
	variants map[uuid.UUID]struct{}
}

// sortedIPs returns the accumulators in deterministic IP order.
func sortedIPs(ips map[netip.Addr]*ipAccum) []*ipAccum {
	keys := make([]netip.Addr, 0, len(ips))
	for k := range ips {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]*ipAccum, 0, len(keys))
	for _, k := range keys {
		out = append(out, ips[k])
	}
	return out
}

func applyComposition(c *Composition, category string) {
	switch category {
	case "mobile":
		c.Mobile++
	case "residential":
		c.Residential++
	case "datacenter":
		c.Datacenter++
	default:
		c.Unknown++
	}
}

func normalizedCategory(value string) string {
	switch value {
	case "mobile", "residential", "datacenter":
		return value
	default:
		return "unknown"
	}
}

func newRiskHistogram() []RiskBucket {
	buckets := make([]RiskBucket, 10)
	for i := range buckets {
		min := i * 10
		max := min + 9
		if i == 9 {
			max = 100
		}
		buckets[i] = RiskBucket{Label: labelFor(min, max), Min: min, Max: max}
	}
	return buckets
}

func labelFor(min, max int) string {
	return strconv.Itoa(min) + "-" + strconv.Itoa(max)
}

func addRisk(buckets []RiskBucket, score int) {
	if score < 0 || score > 100 {
		return
	}
	bucket := score / 10
	if bucket == 10 {
		bucket = 9
	}
	buckets[bucket].Count++
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/variation/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/variation/run.go internal/variation/report.go internal/variation/report_test.go
git commit -m "feat: add variation run domain and pool report builder"
```

---

### Task 6: Postgres migration + sqlc queries for runs

**Files:**
- Create: `migrations/postgres/20260806120000_variation_runs.sql`
- Create: `internal/db/query/runs.sql`
- Modify: `internal/db/query/sessions.sql` (exclude children from `Sessions`; add `SessionsByRun`)
- Generated (via `make sqlc-generate`): `internal/db/*.sql.go`, `internal/db/models.go`, `internal/db/querier.go`

**Interfaces:**
- Produces sqlc methods: `InsertRun`, `InsertRunSession`, `Runs`, `RunByID`, `RunSessions`, `RunIPObservations`, `DeleteRun`, `SessionsByRun`; modified `Sessions`.

- [ ] **Step 1: Write the migration**

`migrations/postgres/20260806120000_variation_runs.sql`:

```sql
-- +goose Up
CREATE TABLE variation_runs (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    template_ciphertext bytea NOT NULL,
    template_nonce bytea NOT NULL,
    template_display text NOT NULL,
    axes jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sampling_sessions ADD COLUMN run_id uuid REFERENCES variation_runs(id) ON DELETE CASCADE;
ALTER TABLE sampling_sessions ADD COLUMN variant_params jsonb;
ALTER TABLE sampling_sessions ADD COLUMN cell_key text;
CREATE INDEX sampling_sessions_run_idx ON sampling_sessions (run_id);

-- +goose Down
DROP INDEX IF EXISTS sampling_sessions_run_idx;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS cell_key;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS variant_params;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS run_id;
DROP TABLE IF EXISTS variation_runs;
```

- [ ] **Step 2: Modify `internal/db/query/sessions.sql`**

Change the `Sessions` query's FROM clause to exclude children by adding a WHERE:

```sql
-- name: Sessions :many
SELECT
  s.id, s.name, s.proxy_ciphertext, s.proxy_nonce, s.proxy_display, s.mode,
  s.cadence_seconds, s.probes_per_sample, s.probe_target, s.dial_timeout_ms,
  s.max_samples, s.max_duration_seconds, s.status, s.samples_taken, s.probes_ok,
  s.probes_total, s.distinct_ips, s.last_sample_at, CAST(COALESCE(s.last_primary_ip::text, '') AS text) AS last_primary_ip,
  COALESCE(r.category, '') AS last_category, s.last_rtt_ms, s.last_error,
  s.created_at, s.started_at, s.stopped_at, s.sequence_offset
FROM sampling_sessions AS s
LEFT JOIN ip_reputation_cache AS r ON r.ip = s.last_primary_ip
WHERE s.run_id IS NULL
ORDER BY s.created_at DESC;
```

(Do NOT change `SessionByID`, `RunningSessions`, or `InsertSession` — children still resume normally and are addressable individually; `run_id` defaults NULL on the existing insert.)

- [ ] **Step 3: Write `internal/db/query/runs.sql`**

```sql
-- name: InsertRun :exec
INSERT INTO variation_runs (id, name, template_ciphertext, template_nonce, template_display, axes, created_at)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(template_ciphertext), sqlc.arg(template_nonce),
  sqlc.arg(template_display), sqlc.arg(axes), sqlc.arg(created_at));

-- name: InsertRunSession :exec
INSERT INTO sampling_sessions (
  id, name, proxy_ciphertext, proxy_nonce, proxy_display, mode,
  cadence_seconds, probes_per_sample, probe_target, dial_timeout_ms,
  max_samples, max_duration_seconds, status, samples_taken, probes_ok,
  probes_total, distinct_ips, last_sample_at, last_primary_ip, last_rtt_ms,
  last_error, created_at, started_at, stopped_at, sequence_offset,
  run_id, variant_params, cell_key
) VALUES (
  sqlc.arg(id), sqlc.arg(name), sqlc.arg(proxy_ciphertext), sqlc.arg(proxy_nonce),
  sqlc.arg(proxy_display), sqlc.arg(mode), sqlc.arg(cadence_seconds),
  sqlc.arg(probes_per_sample), sqlc.arg(probe_target), sqlc.arg(dial_timeout_ms),
  sqlc.narg(max_samples), sqlc.narg(max_duration_seconds), sqlc.arg(status),
  sqlc.arg(samples_taken), sqlc.arg(probes_ok), sqlc.arg(probes_total),
  sqlc.arg(distinct_ips), sqlc.narg(last_sample_at),
  NULLIF(sqlc.arg(last_primary_ip)::text, '')::inet, sqlc.narg(last_rtt_ms),
  NULLIF(sqlc.arg(last_error)::text, ''), sqlc.arg(created_at),
  sqlc.narg(started_at), sqlc.narg(stopped_at), sqlc.arg(sequence_offset),
  sqlc.arg(run_id), sqlc.arg(variant_params), sqlc.arg(cell_key)
);

-- name: Runs :many
SELECT
  run.id, run.name, run.template_ciphertext, run.template_nonce, run.template_display,
  run.axes, run.created_at,
  COUNT(s.id) AS variant_count,
  COALESCE(SUM(s.distinct_ips), 0)::bigint AS distinct_ips,
  COUNT(*) FILTER (WHERE s.status = 'running') AS running_count,
  COUNT(*) FILTER (WHERE s.status = 'finished') AS finished_count
FROM variation_runs AS run
LEFT JOIN sampling_sessions AS s ON s.run_id = run.id
GROUP BY run.id
ORDER BY run.created_at DESC;

-- name: RunByID :one
SELECT
  run.id, run.name, run.template_ciphertext, run.template_nonce, run.template_display,
  run.axes, run.created_at,
  COUNT(s.id) AS variant_count,
  COALESCE(SUM(s.distinct_ips), 0)::bigint AS distinct_ips,
  COUNT(*) FILTER (WHERE s.status = 'running') AS running_count,
  COUNT(*) FILTER (WHERE s.status = 'finished') AS finished_count
FROM variation_runs AS run
LEFT JOIN sampling_sessions AS s ON s.run_id = run.id
WHERE run.id = sqlc.arg(id)
GROUP BY run.id;

-- name: RunSessions :many
SELECT
  s.id, s.name, COALESCE(s.variant_params, '{}'::jsonb) AS variant_params,
  COALESCE(s.cell_key, '') AS cell_key, s.status,
  s.samples_taken, s.probes_ok, s.probes_total, s.distinct_ips
FROM sampling_sessions AS s
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY s.created_at ASC;

-- name: SessionsByRun :many
SELECT s.id
FROM sampling_sessions AS s
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY s.created_at ASC;

-- name: RunIPObservations :many
SELECT
  si.session_id, si.ip::text AS ip, si.hit_count, si.first_seen, si.last_seen,
  CAST(COALESCE(r.ip::text, '') AS text) AS reputation_ip, r.country, r.region, r.city, r.isp, r.asn,
  r.is_mobile, r.ipapi_proxy, r.ipapi_hosting, r.pc_type, r.pc_proxy,
  r.risk_score, r.greynoise_class, r.sfs_appears, r.sfs_frequency,
  r.dnsbl_listed, r.dnsbl_hits, r.category, r.raw, r.first_seen AS reputation_first_seen,
  r.refreshed_at
FROM session_ips AS si
JOIN sampling_sessions AS s ON s.id = si.session_id
LEFT JOIN ip_reputation_cache AS r ON r.ip = si.ip
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY si.session_id, si.last_seen DESC;

-- name: DeleteRun :exec
DELETE FROM variation_runs WHERE id = sqlc.arg(id);
```

- [ ] **Step 4: Regenerate and verify build**

Run: `make sqlc-generate && go build ./...`
Expected: builds. If sqlc reports a type error on `variant_params jsonb`, confirm the generated Go type is `[]byte` (pgx maps jsonb to `[]byte`); the store mapping in Task 7 assumes `[]byte`.

- [ ] **Step 5: Run existing db tests (skip-aware)**

Run: `go test ./internal/db/ -run Session -v`
Expected: PASS or SKIP (no Postgres). No compile errors.

- [ ] **Step 6: Commit**

```bash
git add migrations/postgres/20260806120000_variation_runs.sql internal/db/query/ internal/db/*.go
git commit -m "feat: add variation run schema and queries"
```

---

### Task 7: db store — run methods

**Files:**
- Create: `internal/db/runs.go`
- Test: `internal/db/runs_integration_test.go`

**Interfaces:**
- Consumes: generated queries (Task 6), `variation.Run/ChildSession/RunSummary/VariantSession/IPObservation/Store` (Task 5), existing mapping helpers in `internal/db/store.go` (`insertSessionParams`, `reputationFromValues`, `parseAddr`, `timestamp`).
- Produces: `internal/db.Store` implements `variation.Store` (compile-time assert `var _ variation.Store = (*Store)(nil)`).

- [ ] **Step 1: Write the failing integration test**

`internal/db/runs_integration_test.go` (mirror the skip pattern from `store_integration_test.go` — reuse its `testStore(t)` helper):

```go
package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func TestCreateRunPersistsChildren(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	run := variation.Run{
		ID: uuid.New(), Name: "poolcheck", TemplateCiphertext: []byte("c"),
		TemplateNonce: []byte("n"), TemplateDisplay: "gate:1080",
		Axes: json.RawMessage(`{"country":{"kind":"list","values":["de"]}}`),
		CreatedAt: nowUTC(),
	}
	child := testSession()
	child.ID = uuid.New()
	children := []variation.ChildSession{{
		Session: child, Params: json.RawMessage(`{"country":"de"}`), CellKey: `{"country":"de"}`,
	}}
	if err := store.CreateRun(ctx, run, children); err != nil {
		t.Fatal(err)
	}

	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.VariantCount != 1 {
		t.Fatalf("variant count = %d, want 1", summary.VariantCount)
	}
	if summary.Status != variation.RunRunning {
		t.Fatalf("status = %q, want running", summary.Status)
	}

	sessions, err := store.RunSessions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].CellKey != `{"country":"de"}` {
		t.Fatalf("run sessions = %#v", sessions)
	}

	// Child is excluded from the standalone session list.
	standalone, err := store.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range standalone {
		if s.ID == child.ID {
			t.Fatal("child session leaked into standalone Sessions()")
		}
	}
}

func TestRunStatusDerivation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run := variation.Run{ID: uuid.New(), Name: "r", TemplateCiphertext: []byte("c"), TemplateNonce: []byte("n"), TemplateDisplay: "g", Axes: json.RawMessage(`{}`), CreatedAt: nowUTC()}
	stopped := testSession()
	stopped.ID = uuid.New()
	stopped.Status = session.StatusStopped
	if err := store.CreateRun(ctx, run, []variation.ChildSession{{Session: stopped, Params: json.RawMessage(`{}`), CellKey: `{}`}}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != variation.RunStopped {
		t.Fatalf("status = %q, want stopped", summary.Status)
	}
}
```

> Implementer note: reuse the existing `testSession()`, `testStore(t)`, and any `nowUTC()` helper already in `internal/db/store_integration_test.go`. If `nowUTC` does not exist, add `func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run Run -v`
Expected: FAIL to compile (`store.CreateRun` undefined) — or SKIP if no Postgres, in which case verify via Step 4 build.

- [ ] **Step 3: Implement `internal/db/runs.go`**

```go
package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func (s *Store) CreateRun(ctx context.Context, run variation.Run, children []variation.ChildSession) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.InsertRun(ctx, InsertRunParams{
		ID: run.ID, Name: run.Name, TemplateCiphertext: run.TemplateCiphertext,
		TemplateNonce: run.TemplateNonce, TemplateDisplay: run.TemplateDisplay,
		Axes: run.Axes, CreatedAt: timestamp(run.CreatedAt),
	}); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	for _, child := range children {
		base := insertSessionParams(child.Session)
		if err := q.InsertRunSession(ctx, InsertRunSessionParams{
			ID: base.ID, Name: base.Name, ProxyCiphertext: base.ProxyCiphertext,
			ProxyNonce: base.ProxyNonce, ProxyDisplay: base.ProxyDisplay, Mode: base.Mode,
			CadenceSeconds: base.CadenceSeconds, ProbesPerSample: base.ProbesPerSample,
			ProbeTarget: base.ProbeTarget, DialTimeoutMs: base.DialTimeoutMs,
			MaxSamples: base.MaxSamples, MaxDurationSeconds: base.MaxDurationSeconds,
			Status: base.Status, SamplesTaken: base.SamplesTaken, ProbesOk: base.ProbesOk,
			ProbesTotal: base.ProbesTotal, DistinctIps: base.DistinctIps,
			LastSampleAt: base.LastSampleAt, LastPrimaryIp: base.LastPrimaryIp,
			LastRttMs: base.LastRttMs, LastError: base.LastError, CreatedAt: base.CreatedAt,
			StartedAt: base.StartedAt, StoppedAt: base.StoppedAt, SequenceOffset: base.SequenceOffset,
			RunID: run.ID, VariantParams: child.Params, CellKey: child.CellKey,
		}); err != nil {
			return fmt.Errorf("insert run session: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create run: %w", err)
	}
	return nil
}

func (s *Store) Runs(ctx context.Context) ([]variation.RunSummary, error) {
	rows, err := s.q.Runs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	result := make([]variation.RunSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, variation.RunSummary{
			Run: variation.Run{
				ID: row.ID, Name: row.Name, TemplateCiphertext: row.TemplateCiphertext,
				TemplateNonce: row.TemplateNonce, TemplateDisplay: row.TemplateDisplay,
				Axes: row.Axes, CreatedAt: row.CreatedAt.Time,
			},
			VariantCount: int(row.VariantCount), DistinctIPs: int(row.DistinctIps),
			Status: deriveRunStatus(row.VariantCount, row.RunningCount, row.FinishedCount),
		})
	}
	return result, nil
}

func (s *Store) RunByID(ctx context.Context, id uuid.UUID) (variation.RunSummary, error) {
	row, err := s.q.RunByID(ctx, id)
	if err == pgx.ErrNoRows {
		return variation.RunSummary{}, variation.ErrRunNotFound
	}
	if err != nil {
		return variation.RunSummary{}, fmt.Errorf("run by id: %w", err)
	}
	return variation.RunSummary{
		Run: variation.Run{
			ID: row.ID, Name: row.Name, TemplateCiphertext: row.TemplateCiphertext,
			TemplateNonce: row.TemplateNonce, TemplateDisplay: row.TemplateDisplay,
			Axes: row.Axes, CreatedAt: row.CreatedAt.Time,
		},
		VariantCount: int(row.VariantCount), DistinctIPs: int(row.DistinctIps),
		Status: deriveRunStatus(row.VariantCount, row.RunningCount, row.FinishedCount),
	}, nil
}

func (s *Store) RunSessions(ctx context.Context, id uuid.UUID) ([]variation.VariantSession, error) {
	rows, err := s.q.RunSessions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("run sessions: %w", err)
	}
	result := make([]variation.VariantSession, 0, len(rows))
	for _, row := range rows {
		result = append(result, variation.VariantSession{
			SessionID: row.ID, Name: row.Name, Params: row.VariantParams, CellKey: row.CellKey,
			Status: session.Status(row.Status),
			Snapshot: session.Snapshot{
				SamplesTaken: int(row.SamplesTaken), ProbesOK: row.ProbesOk,
				ProbesTotal: row.ProbesTotal, DistinctIPs: int(row.DistinctIps),
			},
		})
	}
	return result, nil
}

func (s *Store) RunIPObservations(ctx context.Context, id uuid.UUID) ([]variation.IPObservation, error) {
	rows, err := s.q.RunIPObservations(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("run ip observations: %w", err)
	}
	result := make([]variation.IPObservation, 0, len(rows))
	for _, row := range rows {
		ip, err := parseAddr(row.Ip, "run ip")
		if err != nil {
			return nil, err
		}
		obs := variation.IPObservation{
			SessionID: row.SessionID, IP: ip, HitCount: row.HitCount,
			FirstSeen: row.FirstSeen.Time, LastSeen: row.LastSeen.Time,
		}
		if row.ReputationIp != "" {
			reputation, err := reputationFromValues(reputationValues{
				IP: row.ReputationIp, Country: row.Country, Region: row.Region, City: row.City,
				ISP: row.Isp, ASN: row.Asn, IsMobile: row.IsMobile, IPAPIProxy: row.IpapiProxy,
				IPAPIHosting: row.IpapiHosting, ProxyCheckType: row.PcType,
				ProxyCheckProxy: row.PcProxy, RiskScore: row.RiskScore,
				GreyNoiseClass: row.GreynoiseClass, SFSAppears: row.SfsAppears,
				SFSFrequency: row.SfsFrequency, DNSBLListed: row.DnsblListed,
				DNSBLHits: row.DnsblHits, Category: row.Category, Raw: row.Raw,
				FirstSeen: row.ReputationFirstSeen, RefreshedAt: row.RefreshedAt,
			})
			if err != nil {
				return nil, err
			}
			obs.Reputation = &reputation
		}
		result = append(result, obs)
	}
	return result, nil
}

func (s *Store) DeleteRun(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteRun(ctx, id); err != nil {
		return fmt.Errorf("delete run: %w", err)
	}
	return nil
}

// deriveRunStatus mirrors the spec: running if any child runs, else finished if
// every child finished, else stopped. Empty runs report stopped.
func deriveRunStatus(variantCount, runningCount, finishedCount int64) variation.RunStatus {
	if runningCount > 0 {
		return variation.RunRunning
	}
	if variantCount > 0 && finishedCount == variantCount {
		return variation.RunFinished
	}
	return variation.RunStopped
}

var _ variation.Store = (*Store)(nil)
```

> Implementer note: the exact generated field names (`row.VariantCount`, `row.RunningCount`, `row.DistinctIps`, `row.FirstSeen`, `row.LastSeen`, `InsertRunSessionParams.VariantParams`, etc.) come from sqlc; confirm them in `internal/db/runs.sql.go` after Task 6 regeneration and adjust casing to match. `parseAddr`, `reputationFromValues`, `reputationValues`, and `timestamp` all live in `store.go` in the same package — no extra imports needed for them.

- [ ] **Step 4: Build and run**

Run: `go build ./... && go test ./internal/db/ -run Run -v`
Expected: builds; test PASS or SKIP.

- [ ] **Step 5: Commit**

```bash
git add internal/db/runs.go internal/db/runs_integration_test.go
git commit -m "feat: implement variation run store methods"
```

---

### Task 8: ClickHouse reader — series across sessions

**Files:**
- Modify: `internal/ch/reader.go`
- Test: `internal/ch/reader_integration_test.go`

**Interfaces:**
- Produces: `func (r *Reader) SeriesForSessions(ctx context.Context, sessionIDs []uuid.UUID, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error)` — same aggregation as `Series` but over `session_id IN (?)`. Empty `sessionIDs` returns `([]SeriesPoint{}, nil)` without querying.

- [ ] **Step 1: Write the failing test**

Add to `internal/ch/reader_integration_test.go` (follow the file's existing skip/setup helpers — reuse whatever `newTestReader`/insert helper it defines):

```go
func TestSeriesForSessionsAggregatesAcrossSessions(t *testing.T) {
	reader, insert := newReaderWithSamples(t) // existing helper in this file
	s1, s2 := uuid.New(), uuid.New()
	base := time.Now().UTC().Truncate(time.Minute)
	insert(s1, base, /* probes_ok */ 1, 1)
	insert(s2, base, 1, 1)

	points, err := reader.SeriesForSessions(context.Background(), []uuid.UUID{s1, s2}, base.Add(-time.Hour), base.Add(time.Hour), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("expected aggregated series points across both sessions")
	}
}

func TestSeriesForSessionsEmptyIDs(t *testing.T) {
	reader, _ := newReaderWithSamples(t)
	points, err := reader.SeriesForSessions(context.Background(), nil, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 0 {
		t.Fatalf("expected no points for empty ids, got %d", len(points))
	}
}
```

> Implementer note: adapt `newReaderWithSamples`/`insert` to the actual helper names in `reader_integration_test.go`. If none exist, follow the writer-based insertion the file already uses to seed `sample_events`. These tests SKIP without ClickHouse.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ch/ -run SeriesForSessions -v`
Expected: FAIL to compile (`SeriesForSessions` undefined) or SKIP without ClickHouse (verify via build in Step 4).

- [ ] **Step 3: Implement**

Add to `internal/ch/reader.go`:

```go
// SeriesForSessions aggregates the success/latency/composition series across a
// set of sessions, used for variation-run pool reports.
func (r *Reader) SeriesForSessions(ctx context.Context, sessionIDs []uuid.UUID, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	if from.After(to) {
		return nil, ErrInvalidRange
	}
	if bucket <= 0 {
		return nil, ErrInvalidBucket
	}
	if len(sessionIDs) == 0 {
		return []SeriesPoint{}, nil
	}
	bucketSeconds := int64(bucket / time.Second)
	if bucketSeconds == 0 {
		bucketSeconds = 1
	}
	const query = `SELECT
		toStartOfInterval(sampled_at, toIntervalSecond(?)) AS bucket,
		if(sum(probes_attempted) = 0, 0, sum(probes_ok) / sum(probes_attempted)) AS success_rate,
		if(countIf(probes_ok > 0) = 0, 0, quantileExactIf(0.5)(rtt_med_ms, probes_ok > 0)) AS latency_p50_ms,
		if(countIf(probes_ok > 0) = 0, 0, quantileExactIf(0.95)(rtt_med_ms, probes_ok > 0)) AS latency_p95_ms,
		avg(distinct_ips) AS distinct_per_sample,
		countIf(ip_changed > 0) AS ip_changes,
		countIf(primary_category = 'mobile') AS mobile,
		countIf(primary_category = 'residential') AS residential,
		countIf(primary_category = 'datacenter') AS datacenter,
		countIf(primary_category = 'unknown') AS unknown
	FROM sample_events
	WHERE session_id IN (?) AND sampled_at >= ? AND sampled_at <= ?
	GROUP BY bucket
	ORDER BY bucket`
	rows, err := r.conn.Query(ctx, query, bucketSeconds, sessionIDs, from, to)
	if err != nil {
		return nil, fmt.Errorf("query run series: %w", err)
	}
	defer rows.Close()
	points := []SeriesPoint{}
	for rows.Next() {
		var point SeriesPoint
		var latencyP50MS, latencyP95MS uint32
		if err := rows.Scan(
			&point.At, &point.SuccessRate, &latencyP50MS, &latencyP95MS,
			&point.DistinctPerSample, &point.IPChanges,
			&point.Mobile, &point.Residential, &point.Datacenter, &point.Unknown,
		); err != nil {
			return nil, fmt.Errorf("scan run series: %w", err)
		}
		point.LatencyP50MS = float64(latencyP50MS)
		point.LatencyP95MS = float64(latencyP95MS)
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run series: %w", err)
	}
	return points, nil
}
```

- [ ] **Step 4: Build and run**

Run: `go build ./... && go test ./internal/ch/ -run SeriesForSessions -v`
Expected: builds; test PASS or SKIP.

- [ ] **Step 5: Commit**

```bash
git add internal/ch/reader.go internal/ch/reader_integration_test.go
git commit -m "feat: add ClickHouse series across sessions"
```

---

### Task 9: OpenAPI spec — run schemas and paths

**Files:**
- Modify: `api/openapi.yaml`
- Generated (via `make openapi-generate`): `internal/api/openapi/openapi.gen.go`

**Interfaces:**
- Produces generated Go types + strict-server methods for: `createRun`, `listRuns`, `runByID`, `deleteRun`, `stopRun`, `reenableRun`, `runReport`, `exportRunCSV`.

- [ ] **Step 1: Add paths under `paths:`** (after the `/api/sessions/...` block, before `/healthz`)

```yaml
  /api/runs:
    post:
      operationId: createRun
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/CreateRunRequest'}
      responses:
        '201':
          description: Run created and started
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Run'}
        '400': {$ref: '#/components/responses/BadRequest'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
    get:
      operationId: listRuns
      responses:
        '200':
          description: Runs
          content:
            application/json:
              schema:
                type: array
                items: {$ref: '#/components/schemas/Run'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
  /api/runs/{id}:
    parameters:
      - $ref: '#/components/parameters/RunID'
    get:
      operationId: runByID
      responses:
        '200':
          description: Run detail
          content:
            application/json:
              schema: {$ref: '#/components/schemas/RunDetail'}
        '400': {$ref: '#/components/responses/BadRequest'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
    delete:
      operationId: deleteRun
      responses:
        '204': {description: Run deleted}
        '400': {$ref: '#/components/responses/BadRequest'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
  /api/runs/{id}/stop:
    parameters:
      - $ref: '#/components/parameters/RunID'
    post:
      operationId: stopRun
      responses:
        '200':
          description: Run stopped
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Run'}
        '400': {$ref: '#/components/responses/BadRequest'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
  /api/runs/{id}/reenable:
    parameters:
      - $ref: '#/components/parameters/RunID'
    post:
      operationId: reenableRun
      responses:
        '200':
          description: Run re-enabled
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Run'}
        '400': {$ref: '#/components/responses/BadRequest'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
  /api/runs/{id}/report:
    parameters:
      - $ref: '#/components/parameters/RunID'
    get:
      operationId: runReport
      responses:
        '200':
          description: Pool report
          content:
            application/json:
              schema: {$ref: '#/components/schemas/RunReport'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
  /api/runs/{id}/export.csv:
    parameters:
      - $ref: '#/components/parameters/RunID'
    get:
      operationId: exportRunCSV
      responses:
        '200':
          description: Deduped pool IP CSV
          headers:
            Content-Disposition: {schema: {type: string}}
          content:
            text/csv:
              schema: {type: string, format: binary}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
```

- [ ] **Step 2: Add the `RunID` parameter** under `components.parameters` (next to `SessionID`):

```yaml
    RunID:
      name: id
      in: path
      required: true
      schema: {type: string, format: uuid}
```

- [ ] **Step 3: Add schemas** under `components.schemas` (reuse `PoolComposition`, `RiskBucket`, `SeriesPoint`, `IPRow` already defined):

```yaml
    AxisSpec:
      type: object
      additionalProperties: false
      required: [kind]
      properties:
        kind: {type: string, enum: [list, range, random]}
        values:
          type: array
          items: {type: string}
        from: {type: integer}
        to: {type: integer}
        count: {type: integer, minimum: 1}
        length: {type: integer, minimum: 1}
    CreateRunRequest:
      type: object
      additionalProperties: false
      required: [name, template, axes, mode, cadence_seconds]
      properties:
        name: {type: string, minLength: 1, maxLength: 100}
        template: {type: string, minLength: 1, writeOnly: true}
        axes:
          type: object
          additionalProperties: {$ref: '#/components/schemas/AxisSpec'}
        mode: {type: string, enum: [sticky, pool]}
        cadence_seconds: {type: integer, minimum: 1, maximum: 2147483647}
        probes_per_sample: {type: integer, minimum: 1, maximum: 255}
        probe_target: {type: string, format: uri}
        dial_timeout_ms: {type: integer, minimum: 100, maximum: 2147483647, default: 10000}
        max_samples: {type: integer, minimum: 1, maximum: 2147483647, nullable: true}
        max_duration_seconds: {type: integer, minimum: 1, maximum: 2147483647, nullable: true}
    Run:
      type: object
      required: [id, name, template_display, status, variant_count, distinct_ips, created_at]
      properties:
        id: {type: string, format: uuid}
        name: {type: string}
        template_display: {type: string}
        status: {type: string, enum: [running, stopped, finished]}
        variant_count: {type: integer, minimum: 0}
        distinct_ips: {type: integer, minimum: 0}
        created_at: {type: string, format: date-time}
    VariantSummary:
      type: object
      required: [session_id, name, cell_key, params, status, samples_taken, distinct_ips]
      properties:
        session_id: {type: string, format: uuid}
        name: {type: string}
        cell_key: {type: string}
        params:
          type: object
          additionalProperties: {type: string}
        status: {type: string, enum: [running, stopped, finished]}
        samples_taken: {type: integer, minimum: 0}
        distinct_ips: {type: integer, minimum: 0}
    RunDetail:
      type: object
      required: [run, variants]
      properties:
        run: {$ref: '#/components/schemas/Run'}
        variants:
          type: array
          items: {$ref: '#/components/schemas/VariantSummary'}
    CellReport:
      type: object
      required: [cell_key, params, variant_count, distinct_ips, composition]
      properties:
        cell_key: {type: string}
        params:
          type: object
          additionalProperties: {type: string}
        variant_count: {type: integer, minimum: 0}
        distinct_ips: {type: integer, minimum: 0}
        honor_rate: {type: number, format: double, nullable: true, minimum: 0, maximum: 1}
        composition: {$ref: '#/components/schemas/PoolComposition'}
    RunReport:
      type: object
      required: [distinct_ips, estimated_pool_size, pool_size_lower_bound, composition,
        risk_histogram, flagged_ips, flagged_percent, dnsbl_hit_ips, series, cells, ips]
      properties:
        distinct_ips: {type: integer, minimum: 0}
        estimated_pool_size: {type: integer, minimum: 0}
        pool_size_lower_bound: {type: boolean}
        honor_rate: {type: number, format: double, nullable: true, minimum: 0, maximum: 1}
        composition: {$ref: '#/components/schemas/PoolComposition'}
        risk_histogram:
          type: array
          items: {$ref: '#/components/schemas/RiskBucket'}
        flagged_ips: {type: integer, minimum: 0}
        flagged_percent: {type: number, format: double, minimum: 0, maximum: 100}
        dnsbl_hit_ips: {type: integer, minimum: 0}
        series:
          type: array
          items: {$ref: '#/components/schemas/SeriesPoint'}
        cells:
          type: array
          items: {$ref: '#/components/schemas/CellReport'}
        ips:
          type: array
          items: {$ref: '#/components/schemas/IPRow'}
```

- [ ] **Step 4: Regenerate and build**

Run: `make openapi-generate && go build ./...`
Expected: `internal/api/openapi/openapi.gen.go` updates; build fails only because `*Server` does not yet implement the new strict methods (fixed in Tasks 10–12). Confirm the generated file contains `CreateRunRequestObject`, `RunReport`, etc.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml internal/api/openapi/openapi.gen.go
git commit -m "feat: add variation run API schemas"
```

---

### Task 10: run API — create, list, detail

**Files:**
- Create: `internal/api/runs.go`
- Modify: `internal/api/server.go` (add `runStore variation.Store` + `maxVariants int` fields; extend `NewServer`)
- Test: `internal/api/runs_test.go`

**Interfaces:**
- Consumes: `variation.Store` (Task 5/7), `variation.Expand`/`ParseTemplate`/`DefaultRandString` (Tasks 2–3), existing `Control`, `crypto.Cipher`, `proxydial.Display`.
- Produces: `Server.CreateRun`, `Server.ListRuns`, `Server.RunByID`; `NewServer(store, control, cipher, defaults, runStore, maxVariants, readers...)`.

- [ ] **Step 1: Extend `Server` and `NewServer` in `server.go`**

Add fields to the `Server` struct:

```go
	runStore    variation.Store
	maxVariants int
```

Change `NewServer` signature and body:

```go
func NewServer(store session.Store, control Control, cipher *cryptox.Cipher, defaults Defaults, runStore variation.Store, maxVariants int, readers ...Reader) *Server {
	// ...existing default fills...
	if maxVariants <= 0 {
		maxVariants = 128
	}
	// ...existing reader pick...
	return &Server{store: store, control: control, cipher: cipher, reader: reader, defaults: defaults, now: time.Now, runStore: runStore, maxVariants: maxVariants}
}
```

Add the import `"github.com/timo972/proxy-sampler/internal/variation"`. Update all existing `NewServer(...)` call sites in `server_test.go`, `report_test.go`, `csv_test.go` (and any others) to pass `nil, 128` before the readers argument. (Grep: `grep -rn "NewServer(" internal/api`.)

- [ ] **Step 2: Write the failing test**

`internal/api/runs_test.go`:

```go
package api

import (
	"net/http"
	"testing"
)

func TestCreateRunExpandsVariants(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)

	body := `{"name":"poolcheck","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us","fr"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runStore.runs))
	}
	if got := runStore.children[runStore.runs[0].ID]; len(got) != 3 {
		t.Fatalf("children = %d, want 3", len(got))
	}
	if control.startCount != 3 {
		t.Fatalf("Start calls = %d, want 3", control.startCount)
	}
	// Template stored encrypted, never echoed.
	if runStore.runs[0].TemplateDisplay != "gate.example:1080" {
		t.Fatalf("template display = %q", runStore.runs[0].TemplateDisplay)
	}
	if len(runStore.runs[0].TemplateCiphertext) == 0 {
		t.Fatal("template not encrypted")
	}
}

func TestCreateRunRejectsCapExceeded(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithCap(t, store, runStore, &fakeControl{}, 2)
	body := `{"name":"big","template":"p://u-{a}:pw@gate.example:1080","axes":{"a":{"kind":"range","from":1,"to":10}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

func TestCreateRunRejectsUnmatchedAxis(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	body := `{"name":"x","template":"p://u:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}
```

Add fakes + helpers at the bottom of `runs_test.go`:

```go
import (
	"context"
	"github.com/google/uuid"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/variation"
)

type memoryRunStore struct {
	runs     []variation.Run
	children map[uuid.UUID][]variation.ChildSession
}

func newMemoryRunStore() *memoryRunStore {
	return &memoryRunStore{children: map[uuid.UUID][]variation.ChildSession{}}
}

func (m *memoryRunStore) CreateRun(_ context.Context, run variation.Run, children []variation.ChildSession) error {
	m.runs = append(m.runs, run)
	m.children[run.ID] = children
	return nil
}
func (m *memoryRunStore) Runs(context.Context) ([]variation.RunSummary, error) {
	out := make([]variation.RunSummary, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, variation.RunSummary{Run: r, VariantCount: len(m.children[r.ID]), Status: variation.RunRunning})
	}
	return out, nil
}
func (m *memoryRunStore) RunByID(_ context.Context, id uuid.UUID) (variation.RunSummary, error) {
	for _, r := range m.runs {
		if r.ID == id {
			return variation.RunSummary{Run: r, VariantCount: len(m.children[id]), Status: variation.RunRunning}, nil
		}
	}
	return variation.RunSummary{}, variation.ErrRunNotFound
}
func (m *memoryRunStore) RunSessions(_ context.Context, id uuid.UUID) ([]variation.VariantSession, error) {
	out := []variation.VariantSession{}
	for _, c := range m.children[id] {
		out = append(out, variation.VariantSession{SessionID: c.Session.ID, Name: c.Session.Name, Params: c.Params, CellKey: c.CellKey, Status: c.Session.Status})
	}
	return out, nil
}
func (m *memoryRunStore) RunIPObservations(context.Context, uuid.UUID) ([]variation.IPObservation, error) {
	return nil, nil
}
func (m *memoryRunStore) DeleteRun(_ context.Context, id uuid.UUID) error {
	delete(m.children, id)
	for i, r := range m.runs {
		if r.ID == id {
			m.runs = append(m.runs[:i], m.runs[i+1:]...)
		}
	}
	return nil
}

func testRunHandler(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl) http.Handler {
	return testRunHandlerWithCap(t, store, runStore, control, 128)
}

func testRunHandlerWithCap(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl, cap int) http.Handler {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, control, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, runStore, cap)
	return server.Handler()
}
```

Ensure `fakeControl` records `startCount`. If it does not, add an `int` field and increment it in its `Start` method (in `server_test.go`).

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/api/ -run CreateRun -v`
Expected: FAIL (`Server.CreateRun` undefined / signature mismatch).

- [ ] **Step 4: Implement `internal/api/runs.go` (create/list/detail)**

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/proxydial"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// CreateRun expands the template across axes, persists the run and its child
// sessions in one transaction, then starts each child worker.
func (s *Server) CreateRun(ctx context.Context, request openapi.CreateRunRequestObject) (openapi.CreateRunResponseObject, error) {
	if request.Body == nil || s.runStore == nil || s.cipher == nil || s.control == nil {
		return nil, invalidRequest()
	}
	body := request.Body
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.TrimSpace(body.Template) == "" {
		return nil, invalidRequest()
	}
	if body.Mode != openapi.CreateRunRequestModeSticky && body.Mode != openapi.CreateRunRequestModePool {
		return nil, invalidRequest()
	}
	if !validPersistedInteger(body.CadenceSeconds, 1) {
		return nil, invalidRequest()
	}

	tmpl, err := variation.ParseTemplate(body.Template)
	if err != nil {
		return nil, invalidRequest()
	}
	axes, err := mapAxes(body.Axes)
	if err != nil {
		return nil, invalidRequest()
	}
	variants, err := variation.Expand(tmpl, axes, variation.DefaultRandString, s.maxVariants)
	if err != nil {
		return nil, invalidRequest()
	}

	probes := defaultProbes(openapi.CreateSessionRequestMode(body.Mode))
	if body.ProbesPerSample != nil {
		probes = *body.ProbesPerSample
	}
	if probes < 1 || probes > 255 {
		return nil, invalidRequest()
	}
	probeTarget := s.defaults.ProbeTarget
	if body.ProbeTarget != nil {
		probeTarget = *body.ProbeTarget
	}
	if !validProbeTarget(probeTarget) {
		return nil, invalidRequest()
	}
	dialTimeout := s.defaults.DialTimeout
	if body.DialTimeoutMs != nil {
		if !validPersistedInteger(*body.DialTimeoutMs, 100) {
			return nil, invalidRequest()
		}
		dialTimeout = time.Duration(*body.DialTimeoutMs) * time.Millisecond
	}
	if !validOptionalPersistedInteger(body.MaxSamples) || !validOptionalPersistedInteger(body.MaxDurationSeconds) {
		return nil, invalidRequest()
	}

	templateDisplay, err := proxydial.Display(templateForDisplay(body.Template))
	if err != nil {
		templateDisplay = redactTemplate(body.Template)
	}
	templateCiphertext, templateNonce, err := s.cipher.Encrypt(body.Template)
	if err != nil {
		return nil, internalError()
	}

	now := s.now().UTC()
	runID := uuid.New()
	axesJSON, err := json.Marshal(body.Axes)
	if err != nil {
		return nil, internalError()
	}
	run := variation.Run{
		ID: runID, Name: name, TemplateCiphertext: templateCiphertext, TemplateNonce: templateNonce,
		TemplateDisplay: templateDisplay, Axes: axesJSON, CreatedAt: now,
	}

	children := make([]variation.ChildSession, 0, len(variants))
	for i, variant := range variants {
		display, err := proxydial.Display(variant.URL)
		if err != nil {
			return nil, invalidRequest() // an expanded URL is unparseable
		}
		ciphertext, nonce, err := s.cipher.Encrypt(variant.URL)
		if err != nil {
			return nil, internalError()
		}
		params, err := json.Marshal(variant.Params)
		if err != nil {
			return nil, internalError()
		}
		children = append(children, variation.ChildSession{
			Session: session.Session{
				ID: uuid.New(), Name: variantName(name, variant, i), ProxyCiphertext: ciphertext,
				ProxyNonce: nonce, ProxyDisplay: display, Mode: session.Mode(body.Mode),
				Cadence: time.Duration(body.CadenceSeconds) * time.Second, ProbesPerSample: probes,
				ProbeTarget: probeTarget, DialTimeout: dialTimeout, MaxSamples: cloneInt(body.MaxSamples),
				MaxDuration: secondsPointer(body.MaxDurationSeconds), Status: session.StatusRunning,
				CreatedAt: now, StartedAt: timePointer(now),
			},
			Params: params, CellKey: variant.CellKey,
		})
	}

	if err := s.runStore.CreateRun(ctx, run, children); err != nil {
		return nil, internalError()
	}
	for _, child := range children {
		if err := s.control.Start(ctx, child.Session.ID); err != nil {
			// Best-effort: mark the child stopped so the run reflects reality.
			cleanup := context.WithoutCancel(ctx)
			_ = s.store.Stop(cleanup, child.Session.ID, s.now().UTC())
		}
	}
	summary, err := s.runStore.RunByID(ctx, runID)
	if err != nil {
		return nil, internalError()
	}
	return openapi.CreateRun201JSONResponse(mapRun(summary)), nil
}

// ListRuns returns run summaries with derived status and rollup counts.
func (s *Server) ListRuns(ctx context.Context, _ openapi.ListRunsRequestObject) (openapi.ListRunsResponseObject, error) {
	if s.runStore == nil {
		return nil, internalError()
	}
	runs, err := s.runStore.Runs(ctx)
	if err != nil {
		return nil, internalError()
	}
	result := make(openapi.ListRuns200JSONResponse, 0, len(runs))
	for _, run := range runs {
		result = append(result, mapRun(run))
	}
	return result, nil
}

// RunByID returns a run's summary and its variant child summaries.
func (s *Server) RunByID(ctx context.Context, request openapi.RunByIDRequestObject) (openapi.RunByIDResponseObject, error) {
	if s.runStore == nil {
		return nil, internalError()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, internalError()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	detail := openapi.RunDetail{Run: mapRun(summary), Variants: make([]openapi.VariantSummary, 0, len(variants))}
	for _, v := range variants {
		detail.Variants = append(detail.Variants, mapVariant(v))
	}
	return openapi.RunByID200JSONResponse(detail), nil
}

func mapAxes(in map[string]openapi.AxisSpec) (map[string]variation.AxisSpec, error) {
	out := map[string]variation.AxisSpec{}
	for name, spec := range in {
		axis := variation.AxisSpec{Kind: variation.AxisKind(spec.Kind)}
		switch axis.Kind {
		case variation.AxisList:
			if spec.Values != nil {
				axis.Values = *spec.Values
			}
		case variation.AxisRange:
			if spec.From != nil {
				axis.From = *spec.From
			}
			if spec.To != nil {
				axis.To = *spec.To
			}
		case variation.AxisRandom:
			if spec.Count != nil {
				axis.Count = *spec.Count
			}
			if spec.Length != nil {
				axis.Length = *spec.Length
			}
		default:
			return nil, errors.New("unknown axis kind")
		}
		out[name] = axis
	}
	return out, nil
}

func mapRun(summary variation.RunSummary) openapi.Run {
	return openapi.Run{
		Id: summary.ID, Name: summary.Name, TemplateDisplay: summary.TemplateDisplay,
		Status: openapi.RunStatus(summary.Status), VariantCount: summary.VariantCount,
		DistinctIps: summary.DistinctIPs, CreatedAt: summary.CreatedAt,
	}
}

func mapVariant(v variation.VariantSession) openapi.VariantSummary {
	params := map[string]string{}
	_ = json.Unmarshal(v.Params, &params)
	return openapi.VariantSummary{
		SessionId: v.SessionID, Name: v.Name, CellKey: v.CellKey, Params: params,
		Status: openapi.VariantSummaryStatus(v.Status), SamplesTaken: v.Snapshot.SamplesTaken,
		DistinctIps: v.Snapshot.DistinctIPs,
	}
}

func variantName(runName string, variant variation.Variant, index int) string {
	label := variant.CellKey
	if label == "{}" || label == "" {
		return runName + " #" + strconv.Itoa(index+1)
	}
	return runName + " " + label + " #" + strconv.Itoa(index+1)
}

// templateForDisplay/redactTemplate strip credentials from a template so a
// display string can be derived even with placeholders present.
func templateForDisplay(template string) string { return template }

func redactTemplate(template string) string {
	if at := strings.LastIndex(template, "@"); at >= 0 {
		if scheme := strings.Index(template, "://"); scheme >= 0 && scheme < at {
			return template[:scheme+3] + template[at+1:]
		}
	}
	return template
}

```

> Implementer notes:
> - Add `"strconv"` to the import block (used by `variantName`).
> - `proxydial.Display` may fail on a template that still contains `{placeholder}` in the host/port; `redactTemplate` is the fallback. Verify `proxydial.Display` behavior against a placeholder template and prefer it when it succeeds. Confirm generated enum constant names (`openapi.CreateRunRequestModeSticky`, `openapi.RunStatus`, `openapi.VariantSummaryStatus`) in `openapi.gen.go` and adjust.
> - `defaultProbes` takes `openapi.CreateSessionRequestMode`; the cast from `body.Mode` (a `CreateRunRequestMode`) is via its string value — if the generated types are distinct, compare `string(body.Mode)` instead.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ -run CreateRun -v && go test ./internal/api/ -v`
Expected: PASS (all API tests, including updated `NewServer` call sites).

- [ ] **Step 6: Commit**

```bash
git add internal/api/runs.go internal/api/server.go internal/api/runs_test.go internal/api/server_test.go internal/api/report_test.go internal/api/csv_test.go
git commit -m "feat: add create/list/detail run API handlers"
```

---

### Task 11: run API — lifecycle fan-out (stop, reenable, delete)

**Files:**
- Modify: `internal/api/runs.go`
- Test: `internal/api/runs_test.go`

**Interfaces:**
- Consumes: `Control.Stop/Reenable/Delete`, `variation.Store.RunSessions/DeleteRun/RunByID`.
- Produces: `Server.StopRun`, `Server.ReenableRun`, `Server.DeleteRun`.

- [ ] **Step 1: Write the failing test**

```go
func TestStopRunFansOutToChildren(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)
	create := request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d", create.Code)
	}
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodPost, "/api/runs/"+runID+"/stop", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("stop status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.stopCount != 2 {
		t.Fatalf("Stop calls = %d, want 2", control.stopCount)
	}
}

func TestDeleteRunDeletesChildrenThenRun(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodDelete, "/api/runs/"+runID, "")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.Code)
	}
	if control.deleteCount != 1 {
		t.Fatalf("Delete calls = %d, want 1", control.deleteCount)
	}
	if len(runStore.runs) != 0 {
		t.Fatal("run row not deleted")
	}
}

func TestStopRunNotFound(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	resp := request(t, handler, http.MethodPost, "/api/runs/"+uuid.NewString()+"/stop", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}
```

Ensure `fakeControl` records `stopCount` and `deleteCount` (add int fields + increment in its `Stop`/`Delete`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run "StopRun|DeleteRun" -v`
Expected: FAIL (`Server.StopRun` undefined).

- [ ] **Step 3: Implement**

Append to `internal/api/runs.go`:

```go
// StopRun stops every running child of a run. Children already stopped are
// skipped; the refreshed run summary is returned.
func (s *Server) StopRun(ctx context.Context, request openapi.StopRunRequestObject) (openapi.StopRunResponseObject, error) {
	if err := s.fanOut(ctx, request.Id, s.control.Stop, session.ErrNotRunning); err != nil {
		return nil, err
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	return openapi.StopRun200JSONResponse(mapRun(summary)), nil
}

// ReenableRun re-enables every stopped/finished child of a run.
func (s *Server) ReenableRun(ctx context.Context, request openapi.ReenableRunRequestObject) (openapi.ReenableRunResponseObject, error) {
	if err := s.fanOut(ctx, request.Id, s.control.Reenable, session.ErrAlreadyRunning); err != nil {
		return nil, err
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	return openapi.ReenableRun200JSONResponse(mapRun(summary)), nil
}

// DeleteRun deletes every child (crossing the ClickHouse flush barrier per
// child) and then the run row.
func (s *Server) DeleteRun(ctx context.Context, request openapi.DeleteRunRequestObject) (openapi.DeleteRunResponseObject, error) {
	if s.runStore == nil || s.control == nil {
		return nil, internalError()
	}
	if _, err := s.runStore.RunByID(ctx, request.Id); errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	} else if err != nil {
		return nil, internalError()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	for _, v := range variants {
		if err := s.control.Delete(ctx, v.SessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return nil, internalError()
		}
	}
	if err := s.runStore.DeleteRun(ctx, request.Id); err != nil {
		return nil, internalError()
	}
	return openapi.DeleteRun204Response{}, nil
}

// fanOut applies op to each child, treating idempotentSkip as success.
func (s *Server) fanOut(ctx context.Context, runID uuid.UUID, op func(context.Context, uuid.UUID) error, idempotentSkip error) error {
	if s.runStore == nil || s.control == nil {
		return internalError()
	}
	if _, err := s.runStore.RunByID(ctx, runID); errors.Is(err, variation.ErrRunNotFound) {
		return notFound()
	} else if err != nil {
		return internalError()
	}
	variants, err := s.runStore.RunSessions(ctx, runID)
	if err != nil {
		return internalError()
	}
	for _, v := range variants {
		err := op(ctx, v.SessionID)
		if err == nil || errors.Is(err, idempotentSkip) || errors.Is(err, session.ErrNotFound) {
			continue
		}
		return internalError()
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/runs.go internal/api/runs_test.go internal/api/server_test.go
git commit -m "feat: add run lifecycle fan-out handlers"
```

---

### Task 12: run API — report and CSV export

**Files:**
- Modify: `internal/api/runs.go`
- Create: `internal/api/run_report.go`
- Test: `internal/api/run_report_test.go`

**Interfaces:**
- Consumes: `variation.BuildPoolReport`, `variation.Store.RunSessions/RunIPObservations`, `Reader.SeriesForSessions`, existing CSV streaming helpers.
- Produces: `Server.RunReport`, `Server.ExportRunCSV`.

- [ ] **Step 1: Add `SeriesForSessions` to the `Reader` interface**

In `server.go`, extend the `Reader` interface:

```go
	SeriesForSessions(context.Context, []uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error)
```

This is satisfied by `*ch.Reader` (Task 8). Any test fake implementing `Reader` must add the method (return `nil, nil`).

- [ ] **Step 2: Write the failing test**

```go
package api

import (
	"net/http"
	"testing"
)

func TestRunReportReturnsPoolRollup(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID+"/report", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"estimated_pool_size"`) {
		t.Fatalf("body missing pool report fields: %s", resp.Body.String())
	}
}

func TestRunReportNotFound(t *testing.T) {
	handler := testRunHandlerWithReader(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{}, stubReader{})
	resp := request(t, handler, http.MethodGet, "/api/runs/"+uuid.NewString()+"/report", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}
```

Add helpers to `run_report_test.go`:

```go
import (
	"context"
	"strings"
	"time"
	"github.com/google/uuid"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/ch"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

type stubReader struct{}

func (stubReader) Ping(context.Context) error { return nil }
func (stubReader) Samples(context.Context, uuid.UUID, *time.Time, *time.Time, int) (ch.SamplePage, error) {
	return ch.SamplePage{}, nil
}
func (stubReader) StreamSamples(context.Context, uuid.UUID, *time.Time, *time.Time, func(ch.Event) error) error {
	return nil
}
func (stubReader) Series(context.Context, uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error) {
	return nil, nil
}
func (stubReader) Stickiness(context.Context, uuid.UUID, time.Time, time.Time) (ch.Stickiness, error) {
	return ch.Stickiness{}, nil
}
func (stubReader) PoolGrowth(context.Context, uuid.UUID, time.Time, time.Time) ([]ch.GrowthPoint, error) {
	return nil, nil
}
func (stubReader) SeriesForSessions(context.Context, []uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error) {
	return nil, nil
}

func testRunHandlerWithReader(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl, reader Reader) http.Handler {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, control, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, runStore, 128, reader)
	return server.Handler()
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/api/ -run RunReport -v`
Expected: FAIL (`Server.RunReport` undefined).

- [ ] **Step 4: Implement `internal/api/run_report.go`**

```go
package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// RunReport aggregates a run's children into a deduped pool report.
func (s *Server) RunReport(ctx context.Context, request openapi.RunReportRequestObject) (openapi.RunReportResponseObject, error) {
	if s.runStore == nil || s.reader == nil {
		return nil, dependencyUnavailable()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	observations, err := s.runStore.RunIPObservations(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	pool := variation.BuildPoolReport(variants, observations)

	ids := make([]uuid.UUID, 0, len(variants))
	for _, v := range variants {
		ids = append(ids, v.SessionID)
	}
	from := summary.CreatedAt
	to := s.now().UTC()
	bucket := shortReportBucket
	if to.Sub(from) > shortReportRange {
		bucket = longReportBucket
	}
	series, err := s.reader.SeriesForSessions(ctx, ids, from, to, bucket)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	return openapi.RunReport200JSONResponse(mapRunReport(pool, mapSeries(series))), nil
}

func mapRunReport(pool variation.PoolReport, series []openapi.SeriesPoint) openapi.RunReport {
	report := openapi.RunReport{
		DistinctIps: pool.DistinctIPs, EstimatedPoolSize: pool.EstimatedPoolSize,
		PoolSizeLowerBound: pool.PoolSizeLowerBound, HonorRate: pool.HonorRate,
		Composition: openapi.PoolComposition{
			Mobile: pool.Composition.Mobile, Residential: pool.Composition.Residential,
			Datacenter: pool.Composition.Datacenter, Unknown: pool.Composition.Unknown,
		},
		FlaggedIps: pool.FlaggedIPs, FlaggedPercent: pool.FlaggedPercent, DnsblHitIps: pool.DNSBLHitIPs,
		RiskHistogram: mapRiskBuckets(pool.RiskHistogram), Series: series,
		Cells: mapCells(pool.Cells), Ips: mapPoolIPs(pool.IPs),
	}
	return report
}
```

> Implementer note: `RunReport` takes `mapRunReport(pool variation.PoolReport, series []openapi.SeriesPoint)` — convert the ClickHouse series with the existing `mapSeries` at the call site: `openapi.RunReport200JSONResponse(mapRunReport(pool, mapSeries(series)))`. Implement the small mappers `mapRiskBuckets`, `mapCells`, `mapPoolIPs` converting `variation.RiskBucket/CellReport/IPRow` into their `openapi.*` equivalents — mirror the field-by-field style of `mapRun`. For `mapPoolIPs`, map into the generated `openapi.IPRow` (fields `Ip, Category, Country, Isp, Asn, RiskScore, GreynoiseClass, DnsblListed, DnsblHits, FirstSeen, LastSeen, HitCount`); `variation.IPRow` carries `FirstSeen`/`LastSeen` (sourced from `session_ips` in Task 6), so pass them straight through. `mapCells` maps `honor_rate` (`*float64` → nullable) and `composition`.

- [ ] **Step 5: Implement `ExportRunCSV` in `runs.go`**

```go
// ExportRunCSV streams the deduped pool IP list with reputation columns.
func (s *Server) ExportRunCSV(ctx context.Context, request openapi.ExportRunCSVRequestObject) (openapi.ExportRunCSVResponseObject, error) {
	if s.runStore == nil {
		return nil, dependencyUnavailable()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	observations, err := s.runStore.RunIPObservations(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	pool := variation.BuildPoolReport(variants, observations)

	body := runIPCSV(pool.IPs)
	filename := sanitizedFilename(summary.Name) + "-" + summary.ID.String() + "-pool.csv"
	return runCSVResponse{data: body, contentDisposition: `attachment; filename="` + filename + `"`}, nil
}

type runCSVResponse struct {
	data               []byte
	contentDisposition string
}

func (r runCSVResponse) VisitExportRunCSVResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", r.contentDisposition)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(r.data)
	return err
}

var runCSVHeader = []string{"ip", "category", "country", "isp", "asn", "risk_score", "greynoise_class", "dnsbl_listed", "dnsbl_hits", "hit_count"}

func runIPCSV(rows []variation.IPRow) []byte {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	_ = writer.Write(runCSVHeader)
	for _, row := range rows {
		risk := ""
		if row.RiskScore != nil {
			risk = strconv.Itoa(*row.RiskScore)
		}
		_ = writer.Write([]string{
			row.IP, row.Category, row.Country, row.ISP, row.ASN, risk, row.GreyNoiseClass,
			strconv.FormatBool(row.DNSBLListed), strings.Join(row.DNSBLHits, "|"),
			strconv.FormatInt(row.HitCount, 10),
		})
	}
	writer.Flush()
	return buf.Bytes()
}
```

Add imports to `runs.go`: `"bytes"`, `"encoding/csv"`, `"net/http"` (`"strconv"` is already imported from Task 10).

> Implementer note: the pool IP set is bounded by `MAX_VARIANTS_PER_RUN` × distinct IPs — small enough to buffer, unlike the per-probe session CSV which streams. Confirm the generated visitor method name (`VisitExportRunCSVResponse`) in `openapi.gen.go`.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/runs.go internal/api/run_report.go internal/api/run_report_test.go internal/api/server.go
git commit -m "feat: add run report and pool CSV export"
```

---

### Task 13: main wiring

**Files:**
- Modify: `cmd/app/main.go`
- Test: `cmd/app/main_test.go` (adjust to new `NewServer` signature if it constructs one)

**Interfaces:**
- Consumes: `db.Store` (already implements both `session.Store` and `variation.Store`), `cfg.MaxVariantsPerRun`.

- [ ] **Step 1: Wire the run store and cap into `NewServer`**

In `buildApplication` (around `internal/api.NewServer(...)`), pass the same `core.store` as the run store and the config cap:

```go
				server := api.NewServer(core.store, supervisor, core.cipher, api.Defaults{
					ProbeTarget: cfg.ProbeTargetDefault,
					DialTimeout: 10 * time.Second,
				}, core.store, cfg.MaxVariantsPerRun, reader.reader)
```

`core.store` is `*db.Store`, which now satisfies `variation.Store` (Task 7). No other wiring changes; the supervisor already handles child sessions as ordinary sessions, and `Resume` re-owns them on boot.

- [ ] **Step 2: Build and run the full suite**

Run: `go build ./... && make go-test`
Expected: PASS. Fix any `main_test.go` call sites that construct `api.NewServer` directly.

- [ ] **Step 3: Verify generate-check**

Run: `make generate-check`
Expected: clean (no diff) — confirms sqlc/openapi outputs are committed.

- [ ] **Step 4: Commit**

```bash
git add cmd/app/main.go cmd/app/main_test.go
git commit -m "feat: wire variation run store into the API server"
```

---

### Task 14: web — API client types, hooks, and variant-count preview

**Files:**
- Modify: `web/src/lib/api.ts`
- Create: `web/src/lib/variation.ts`
- Create: `web/src/lib/variation.test.ts`

**Interfaces:**
- Produces:
  - Types `AxisSpec`, `Run`, `VariantSummary`, `RunDetail`, `CreateRunRequest`, `CellReport`, `RunReport`.
  - Hooks `useRuns`, `useRun`, `useRunReport`, `useCreateRun`, `useStopRun`, `useReenableRun`, `useDeleteRun`.
  - `variation.ts`: `export function variantCount(axes: Record<string, AxisSpec>): number` — deterministic count without generating values.

- [ ] **Step 1: Write the failing preview test**

`web/src/lib/variation.test.ts`:

```ts
import { describe, expect, it } from 'vitest'
import { variantCount } from './variation'
import type { AxisSpec } from './api'

describe('variantCount', () => {
  it('multiplies list, range, and random axes', () => {
    const axes: Record<string, AxisSpec> = {
      country: { kind: 'list', values: ['de', 'us', 'fr'] },
      port: { kind: 'range', from: 10000, to: 10004 },
      session: { kind: 'random', count: 2 },
    }
    expect(variantCount(axes)).toBe(3 * 5 * 2)
  })

  it('treats an empty list axis as zero', () => {
    expect(variantCount({ c: { kind: 'list', values: [] } })).toBe(0)
  })

  it('returns 1 for no axes', () => {
    expect(variantCount({})).toBe(1)
  })

  it('counts an inclusive range', () => {
    expect(variantCount({ p: { kind: 'range', from: 1, to: 1 } })).toBe(1)
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run variation`
Expected: FAIL (module not found).

- [ ] **Step 3: Implement `web/src/lib/variation.ts`**

```ts
import type { AxisSpec } from './api'

// variantCount mirrors the backend expansion count without generating values.
export function variantCount(axes: Record<string, AxisSpec>): number {
  let total = 1
  for (const spec of Object.values(axes)) {
    total *= axisSize(spec)
  }
  return total
}

function axisSize(spec: AxisSpec): number {
  switch (spec.kind) {
    case 'list':
      return spec.values?.length ?? 0
    case 'range':
      if (spec.from === undefined || spec.to === undefined || spec.to < spec.from) return 0
      return spec.to - spec.from + 1
    case 'random':
      return spec.count && spec.count > 0 ? spec.count : 0
    default:
      return 0
  }
}
```

- [ ] **Step 4: Add types and hooks to `web/src/lib/api.ts`**

Append types:

```ts
export type AxisKind = 'list' | 'range' | 'random'

export interface AxisSpec {
  kind: AxisKind
  values?: string[]
  from?: number
  to?: number
  count?: number
  length?: number
}

export interface Run {
  id: string
  name: string
  template_display: string
  status: SessionStatus
  variant_count: number
  distinct_ips: number
  created_at: string
}

export interface VariantSummary {
  session_id: string
  name: string
  cell_key: string
  params: Record<string, string>
  status: SessionStatus
  samples_taken: number
  distinct_ips: number
}

export interface RunDetail {
  run: Run
  variants: VariantSummary[]
}

export interface CreateRunRequest {
  name: string
  template: string
  axes: Record<string, AxisSpec>
  mode: SessionMode
  cadence_seconds: number
  probes_per_sample?: number
  probe_target?: string
  dial_timeout_ms?: number
  max_samples?: number | null
  max_duration_seconds?: number | null
}

export interface CellReport {
  cell_key: string
  params: Record<string, string>
  variant_count: number
  distinct_ips: number
  honor_rate?: number | null
  composition: PoolComposition
}

export interface RunReport {
  distinct_ips: number
  estimated_pool_size: number
  pool_size_lower_bound: boolean
  honor_rate?: number | null
  composition: PoolComposition
  risk_histogram: RiskBucket[]
  flagged_ips: number
  flagged_percent: number
  dnsbl_hit_ips: number
  series: SeriesPoint[]
  cells: CellReport[]
  ips: IPRow[]
}
```

Append hooks (mirror the existing session hook patterns):

```ts
export function useRuns() {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['runs'],
    queryFn: () => api<Run[]>('/api/runs'),
    refetchInterval: (query) => visible && query.state.data?.some((run) => run.status === 'running') ? 5000 : false,
    refetchIntervalInBackground: false,
  })
}

export function useRun(id: string) {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['runs', id],
    queryFn: () => api<RunDetail>(`/api/runs/${id}`),
    refetchInterval: (query) => visible && query.state.data?.run.status === 'running' ? 5000 : false,
    refetchIntervalInBackground: false,
    enabled: Boolean(id),
  })
}

export function useRunReport(id: string, running: boolean) {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['run-report', id],
    queryFn: () => api<RunReport>(`/api/runs/${id}/report`),
    refetchInterval: visible && running ? 5000 : false,
    refetchIntervalInBackground: false,
    enabled: Boolean(id),
  })
}

export function useCreateRun() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (request: CreateRunRequest) => api<Run>('/api/runs', { method: 'POST', body: JSON.stringify(request) }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['runs'] }),
  })
}

export function useStopRun() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api<Run>(`/api/runs/${id}/stop`, { method: 'POST' }),
    onSuccess: async (_data, id) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['runs'] }),
        queryClient.invalidateQueries({ queryKey: ['runs', id] }),
        queryClient.invalidateQueries({ queryKey: ['run-report', id] }),
      ])
    },
  })
}

export function useReenableRun() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api<Run>(`/api/runs/${id}/reenable`, { method: 'POST' }),
    onSuccess: async (_data, id) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['runs'] }),
        queryClient.invalidateQueries({ queryKey: ['runs', id] }),
        queryClient.invalidateQueries({ queryKey: ['run-report', id] }),
      ])
    },
  })
}

export function useDeleteRun() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api<void>(`/api/runs/${id}`, { method: 'DELETE' }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['runs'] }),
  })
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd web && npm test -- --run variation && cd web && npm test -- --run api`
Expected: PASS. Also run `cd web && npm run build` to typecheck.

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/api.ts web/src/lib/variation.ts web/src/lib/variation.test.ts
git commit -m "feat: add run API client and variant-count preview"
```

---

### Task 15: web — new-run dialog with axes builder

**Files:**
- Create: `web/src/components/new-run-dialog.tsx`
- Create: `web/src/components/new-run-dialog.test.tsx`

**Interfaces:**
- Consumes: `useCreateRun`, `variantCount`, `AxisSpec`, `CreateRunRequest` (Task 14); existing UI primitives in `web/src/components/ui`.
- Produces: `export function NewRunDialog({ open, onClose }: { open: boolean; onClose: () => void })`. Mirror `NewSessionDialog`'s structure, validation, and accessibility (labels, `aria-invalid`, `aria-describedby`).

- [ ] **Step 1: Write the failing test**

`web/src/components/new-run-dialog.test.tsx` (model on `new-session-dialog.test.tsx`):

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { NewRunDialog } from './new-run-dialog'

function renderDialog() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <NewRunDialog open onClose={() => {}} />
    </QueryClientProvider>,
  )
}

describe('NewRunDialog', () => {
  it('shows a live variant-count preview and blocks over the cap', async () => {
    renderDialog()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Run name'), 'poolcheck')
    await user.type(screen.getByLabelText('Proxy template'), 'socks5h://u-cc-{country}:pw@gate:1080')
    // Add a list axis "country" with 4 values.
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.type(screen.getByLabelText('Axis 1 name'), 'country')
    await user.selectOptions(screen.getByLabelText('Axis 1 kind'), 'list')
    await user.type(screen.getByLabelText('Axis 1 values'), 'de, us, fr, jp')
    expect(await screen.findByText(/4 variants/)).toBeInTheDocument()
  })

  it('requires a name, template, and at least one axis', async () => {
    vi.stubGlobal('fetch', vi.fn())
    renderDialog()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Start run' }))
    expect(await screen.findByText('Name is required')).toBeInTheDocument()
    expect(fetch).not.toHaveBeenCalled()
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run new-run-dialog`
Expected: FAIL (module not found).

- [ ] **Step 3: Implement `new-run-dialog.tsx`**

Build the component following `new-session-dialog.tsx` conventions. Required structure:
- Controlled fields: `name` (label "Run name"), `template` (label "Proxy template"), `mode` select (label "Sampling mode", sticky/pool), `cadence_seconds` (label "Cadence (seconds)").
- An **axes builder**: a list of axis rows, each with:
  - name input (label `Axis {n} name`),
  - kind select (label `Axis {n} kind`, options list/range/random),
  - value inputs shown by kind: list → comma-separated `values` (label `Axis {n} values`); range → `from`/`to` number inputs; random → `count` + optional `length`.
  - "Add axis" and per-row "Remove axis" buttons.
- A live preview line computed with `variantCount(builtAxes)` reading `<span>{count} variants</span>`; when `count > MAX` (import a constant `MAX_VARIANTS = 128`, or better: keep the cap client-side as a constant and show a red invalid state — the server is the source of truth and will still reject), disable the submit button and show an error.
- Advanced (collapsed) section: `probes_per_sample`, `probe_target`, `dial_timeout_ms`, `max_samples`, `max_duration_seconds`, matching `NewSessionDialog`.
- Validation before submit: name non-empty; template non-empty; at least one axis; each list axis has ≥1 value; each range has `from ≤ to`; each random has `count ≥ 1`. Build `axes: Record<string, AxisSpec>` and call `useCreateRun().mutate`. On success call `onClose`.

Keep parsing of comma lists trivial: `values.split(',').map((v) => v.trim()).filter(Boolean)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npm test -- --run new-run-dialog && cd web && npm run build`
Expected: PASS + typecheck clean.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/new-run-dialog.tsx web/src/components/new-run-dialog.test.tsx
git commit -m "feat: add new-run dialog with axes builder"
```

---

### Task 16: web — runs list, run detail page, routing, backlink

**Files:**
- Modify: `web/src/App.tsx` (add `/runs/:id` route; wire run dialog open state)
- Modify: `web/src/pages/home-page.tsx` (Runs section + New run button)
- Create: `web/src/pages/run-page.tsx`
- Create: `web/src/pages/run-page.test.tsx`
- Modify: `web/src/pages/session-page.tsx` (backlink badge when the session was opened from a run — optional, see note)

**Interfaces:**
- Consumes: `useRuns`, `useRun`, `useRunReport`, `useStopRun`, `useReenableRun`, `useDeleteRun` (Task 14); existing report components (`RiskHistogram`, `PoolCompositionChart`, `SuccessRateChart`, `LatencyChart`, `IPTable`).

- [ ] **Step 1: Write the failing run-page test**

`web/src/pages/run-page.test.tsx` (model on `session-page.test.tsx` — mock `fetch` to return a `RunDetail` then a `RunReport`):

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'

import { RunPage } from './run-page'

function mockFetch(detail: unknown, report: unknown) {
  return vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    const body = url.endsWith('/report') ? report : detail
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }))
  })
}

describe('RunPage', () => {
  it('renders the pool rollup stat strip', async () => {
    const detail = { run: { id: 'r1', name: 'poolcheck', template_display: 'gate:1080', status: 'running', variant_count: 3, distinct_ips: 12, created_at: new Date().toISOString() }, variants: [] }
    const report = { distinct_ips: 12, estimated_pool_size: 40, pool_size_lower_bound: false, honor_rate: 1, composition: { mobile: 0, residential: 12, datacenter: 0, unknown: 0 }, risk_histogram: [], flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [], cells: [], ips: [] }
    vi.stubGlobal('fetch', mockFetch(detail, report))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('poolcheck')).toBeInTheDocument())
    expect(await screen.findByText(/40/)).toBeInTheDocument() // estimated pool size
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run run-page`
Expected: FAIL (module not found).

- [ ] **Step 3: Implement `run-page.tsx`**

Structure (mirror `session-page.tsx`):
- Read `:id` via `useParams`. `useRun(id)` for detail, `useRunReport(id, running)` where `running = detail.run.status === 'running'`.
- Header: run name, `SessionStatusBadge` (reuse — status values match), buttons Stop / Re-enable / Delete wired to `useStopRun`/`useReenableRun`/`useDeleteRun`, plus a "Export pool CSV" link to `/api/runs/${id}/export.csv`.
- Stat strip: distinct IPs, estimated pool size (append "≥" / "(lower bound)" when `pool_size_lower_bound`), honor rate (`—` when `null`), flagged %, and a success-rate summary from the series (or omit if empty).
- Charts: `SuccessRateChart`/`LatencyChart` from `report.series`; `PoolCompositionChart` from `report.composition`; `RiskHistogram` from `report.risk_histogram`.
- Per-cell table: render `report.cells` (cell params, variant count, distinct IPs, honor rate, composition) as a simple sortable table.
- Variants table: `detail.variants`, each linking to `/sessions/${session_id}`.
- Deduped pool IPs: reuse `IPTable` with `report.ips` (its `IPRow` shape matches).

- [ ] **Step 4: Add the route and home wiring**

In `App.tsx`, add:

```tsx
<Route path="/runs/:id" element={<Suspense fallback={<main className="route-placeholder" aria-label="Run report loading boundary" />}><RunPage /></Suspense>} />
```

(import `RunPage` lazily like `SessionPage`). Add a `newRunOpen` state alongside `newSessionOpen` and render `<NewRunDialog open={newRunOpen} onClose={() => setNewRunOpen(false)} />`. Pass an `onNewRun` callback to `HomePage`.

In `home-page.tsx`, add a Runs section above/beside the sessions section: `useRuns()` → render a runs table (name → link `/runs/:id`, variant count, status badge, distinct IPs, created) with an empty state and a "New run" button calling `onNewRun`. The existing sessions list is unchanged (the API now excludes children automatically).

- [ ] **Step 5: (Optional) session backlink badge**

Skip cross-linking from child session pages in this task unless trivial: the child's session detail already works standalone. If added later, it needs a `run_id` on the session detail response — out of scope here. Note this explicitly in the PR description rather than expanding scope.

- [ ] **Step 6: Run the full web suite**

Run: `cd web && npm test -- --run && cd web && npm run build`
Expected: PASS + typecheck clean.

- [ ] **Step 7: Commit**

```bash
git add web/src/App.tsx web/src/pages/home-page.tsx web/src/pages/run-page.tsx web/src/pages/run-page.test.tsx
git commit -m "feat: add runs list and run detail page"
```

---

### Task 17: Full-stack verification

**Files:** none (verification only)

- [ ] **Step 1: Backend suite + generate check**

Run: `make go-test && make generate-check`
Expected: PASS, no diff.

- [ ] **Step 2: Web suite + build**

Run: `cd web && npm test -- --run && cd web && npm run build`
Expected: PASS + build.

- [ ] **Step 3: Integration tests (if datastores available)**

Run: `make integration-test`
Expected: PASS, or SKIP where Postgres/ClickHouse are absent. If available, confirms `CreateRun` transaction, run queries, and `SeriesForSessions` against real datastores.

- [ ] **Step 4: Vet + format**

Run: `make vet && make fmt && git diff --exit-code`
Expected: clean.

- [ ] **Step 5: Final commit if fmt changed anything**

```bash
git add -A
git commit -m "chore: gofmt variation run code" || echo "nothing to format"
```

---

## Self-Review Notes

- **Spec coverage:** template+axes+expansion (Tasks 2–3), range/random/list (Task 3), cap via env (Tasks 1, 3, 10), fresh-per-cell randomness (Task 3), data model + 3 session columns + derived status (Tasks 6–7), creation transaction + start fan-out + Resume safety (Tasks 7, 10, 13), API routes incl. stop/reenable/delete/report/export (Tasks 9–12), pool rollup + honor-rate-from-params + Chao1 + per-cell (Tasks 4–5, 12), CH series across children (Task 8), UI runs list + new-run dialog + run detail + preview (Tasks 14–16), CSV export (Task 12), config (Task 1), error handling (Tasks 3, 10–12), testing throughout.
- **Type consistency:** `variation.Store` (Task 5) is implemented in Task 7 and consumed in Tasks 10–12. `IPObservation` and `IPRow` both carry `FirstSeen`/`LastSeen` (Task 5), sourced from `session_ips.first_seen/last_seen` in the Task 6 query and mapped in Task 7 — so the generated `openapi.IPRow`'s required `first_seen`/`last_seen` are satisfied without a placeholder. `ipAccum` is a package-level named type used by both `BuildPoolReport` and `sortedIPs`. The `Reader` interface gains `SeriesForSessions` (Task 12) satisfied by `*ch.Reader` (Task 8); every test fake implementing `Reader` adds that method.
- **Known follow-ups (out of scope):** child-session backlink badge (Task 16 Step 5); `?run_id=` filter on `GET /api/sessions` (spec mentions it; the standalone-exclusion in Task 6 covers the primary need — add the filter only if a UI consumer needs it).
