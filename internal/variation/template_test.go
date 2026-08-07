package variation

import (
	"reflect"
	"strings"
	"testing"
)

func TestRenderRejectsOversizedOutput(t *testing.T) {
	// A placeholder repeated many times, each substituted with a large value,
	// must be rejected rather than allocating an enormous rendered string.
	tmpl, err := ParseTemplate(strings.Repeat("{v}", 1000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpl.Render(map[string]string{"v": strings.Repeat("a", 4096)}); err == nil {
		t.Fatal("expected error: rendered output exceeds the size cap")
	}
}

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
