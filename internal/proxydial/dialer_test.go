package proxydial

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFromURLSupportsProxySchemes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "SOCKS5", raw: "socks5://proxy.example:1080"},
		{name: "SOCKS5 remote DNS with encoded credentials", raw: "socks5h://us%65r:s%65cret@proxy.example:1080"},
		{name: "HTTP CONNECT", raw: "http://proxy.example:8080"},
		{name: "HTTPS CONNECT over IPv6", raw: "https://[2001:db8::1]:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer, err := FromURL(tt.raw, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if dialer == nil {
				t.Fatal("nil dialer")
			}
		})
	}
}

func TestFromURLRejectsMissingPort(t *testing.T) {
	for _, raw := range []string{
		"socks5://proxy.example",
		"http://proxy.example",
		"https://[2001:db8::1]",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := FromURL(raw, time.Second)
			if !strings.Contains(fmt.Sprint(err), "requires host and port") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestFromURLRejectsUnsupportedScheme(t *testing.T) {
	_, err := FromURL("ftp://proxy.example:21", time.Second)
	if !strings.Contains(fmt.Sprint(err), `unsupported proxy scheme "ftp"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDisplayRedactsCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "hostname",
			raw:  "socks5h://us%65r:s%65cret@proxy.example:1080",
			want: "proxy.example:1080",
		},
		{
			name: "IPv6",
			raw:  "http://us%65r:s%65cret@[2001:db8::1]:8080",
			want: "[2001:db8::1]:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			display, err := Display(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if display != tt.want {
				t.Fatalf("display = %q, want %q", display, tt.want)
			}
			if strings.Contains(display, "user") || strings.Contains(display, "secret") {
				t.Fatal("display contains proxy credentials")
			}
		})
	}
}

func TestDisplayRejectsMissingPort(t *testing.T) {
	_, err := Display("http://user:secret@proxy.example")
	if !strings.Contains(fmt.Sprint(err), "requires host and port") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseErrorsDoNotExposeCredentials(t *testing.T) {
	const malformed = "http://sensitive-user:sensitive-password@proxy.example:%"

	_, dialErr := FromURL(malformed, time.Second)
	_, displayErr := Display(malformed)
	for _, err := range []error{dialErr, displayErr} {
		if err == nil {
			t.Fatal("malformed URL accepted")
		}
		if strings.Contains(err.Error(), "sensitive-user") || strings.Contains(err.Error(), "sensitive-password") {
			t.Fatal("parse error exposed proxy credentials")
		}
	}
}
