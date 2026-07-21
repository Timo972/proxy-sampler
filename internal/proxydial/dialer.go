// Package proxydial constructs credential-aware proxy dialers.
package proxydial

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// FromURL builds a context-aware dialer for a supported proxy URL.
func FromURL(raw string, timeout time.Duration) (proxy.ContextDialer, error) {
	u, err := parseURL(raw)
	if err != nil {
		return nil, err
	}

	base := &net.Dialer{Timeout: timeout}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, base)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 dialer lacks context support")
		}
		return contextDialer, nil
	case "http", "https":
		return newConnectDialer(u, base), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// Display returns a credential-free host and port for a proxy URL.
func Display(raw string) (string, error) {
	u, err := parseURL(raw)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(u.Hostname(), u.Port()), nil
}

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("parse proxy URL: invalid URL")
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, errors.New("proxy URL requires host and port")
	}
	return u, nil
}
