package proxydial

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
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

func TestSOCKS5HSendsUnresolvedTargetAsDomainName(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	type socksTarget struct {
		host string
		port uint16
		atyp byte
	}
	targets := make(chan socksTarget, 1)
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()

		greeting := make([]byte, 2)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			serverErrors <- err
			return
		}
		methods := make([]byte, int(greeting[1]))
		if greeting[0] != 5 {
			serverErrors <- fmt.Errorf("SOCKS version = %d", greeting[0])
			return
		}
		if _, err := io.ReadFull(conn, methods); err != nil {
			serverErrors <- err
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			serverErrors <- err
			return
		}

		header := make([]byte, 4)
		if _, err := io.ReadFull(conn, header); err != nil {
			serverErrors <- err
			return
		}
		if header[0] != 5 || header[1] != 1 || header[2] != 0 {
			serverErrors <- errors.New("unexpected SOCKS5 CONNECT header")
			return
		}
		if header[3] != 3 {
			serverErrors <- fmt.Errorf("target ATYP = %d, want domain name", header[3])
			return
		}
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			serverErrors <- err
			return
		}
		host := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, host); err != nil {
			serverErrors <- err
			return
		}
		var portBytes [2]byte
		if _, err := io.ReadFull(conn, portBytes[:]); err != nil {
			serverErrors <- err
			return
		}
		targets <- socksTarget{host: string(host), port: binary.BigEndian.Uint16(portBytes[:]), atyp: header[3]}
		if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
			serverErrors <- err
			return
		}
		serverErrors <- nil
	}()

	dialer, err := FromURL("socks5h://"+listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "does-not-resolve.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := receive(t, targets)
	if target.host != "does-not-resolve.invalid" || target.port != 443 || target.atyp != 3 {
		t.Fatalf("SOCKS target = %#v", target)
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}
