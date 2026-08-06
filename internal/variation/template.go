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
