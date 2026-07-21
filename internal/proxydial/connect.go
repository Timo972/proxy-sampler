package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type connectDialer struct {
	proxyURL *url.URL
	base     *net.Dialer
}

func newConnectDialer(proxyURL *url.URL, base *net.Dialer) *connectDialer {
	return &connectDialer{proxyURL: proxyURL, base: base}
}

func (d *connectDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	conn, err := d.base.DialContext(ctx, "tcp", d.proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = conn.Close()
		}
	}()

	if strings.EqualFold(d.proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxyURL.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("establish proxy TLS: %w", err)
		}
		conn = tlsConn
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}
	if d.proxyURL.User != nil {
		password, _ := d.proxyURL.User.Password()
		credentials := d.proxyURL.User.Username() + ":" + password
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := request.Write(conn); err != nil {
		return nil, fmt.Errorf("write proxy CONNECT request: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("read proxy CONNECT response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy CONNECT returned HTTP status %d", response.StatusCode)
	}

	succeeded = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
