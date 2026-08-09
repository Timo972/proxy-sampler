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

// MaxRenderedBytes bounds a rendered proxy URL. A placeholder repeated many
// times in a template, each substituted with a large axis value, would
// otherwise expand a small capped input into a huge string; a real proxy URL
// is never close to this size.
const MaxRenderedBytes = 8192

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

// Render substitutes every placeholder with its value. It builds the result
// incrementally and aborts once the output would exceed MaxRenderedBytes, so a
// placeholder repeated many times with a large value cannot allocate an
// enormous string before the caller can reject it.
func (t Template) Render(values map[string]string) (string, error) {
	var b strings.Builder
	last := 0
	for _, match := range placeholderPattern.FindAllStringSubmatchIndex(t.raw, -1) {
		start, end, nameStart, nameEnd := match[0], match[1], match[2], match[3]
		b.WriteString(t.raw[last:start])
		name := strings.TrimSpace(t.raw[nameStart:nameEnd])
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("no value for placeholder %q", name)
		}
		b.WriteString(value)
		if b.Len() > MaxRenderedBytes {
			return "", fmt.Errorf("rendered URL exceeds %d bytes", MaxRenderedBytes)
		}
		last = end
	}
	b.WriteString(t.raw[last:])
	if b.Len() > MaxRenderedBytes {
		return "", fmt.Errorf("rendered URL exceeds %d bytes", MaxRenderedBytes)
	}
	return b.String(), nil
}
