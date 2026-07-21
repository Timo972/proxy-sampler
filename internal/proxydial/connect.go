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
	"time"
)

type connectDialer struct {
	proxyURL  *url.URL
	base      *net.Dialer
	tlsConfig *tls.Config
}

func newConnectDialer(proxyURL *url.URL, base *net.Dialer) *connectDialer {
	return &connectDialer{
		proxyURL: proxyURL,
		base:     base,
		tlsConfig: &tls.Config{
			ServerName: proxyURL.Hostname(),
		},
	}
}

func (d *connectDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	handshakeDeadline := connectDeadline(ctx, time.Now(), d.base.Timeout)
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
	if !handshakeDeadline.IsZero() {
		if err := conn.SetDeadline(handshakeDeadline); err != nil {
			return nil, fmt.Errorf("set proxy CONNECT deadline: %w", err)
		}
	}
	transportConn := conn
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = transportConn.Close()
	})
	cancellationStopped := false
	defer func() {
		if !cancellationStopped {
			stopCancellation()
		}
	}()

	if strings.EqualFold(d.proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, d.tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, connectOperationError(ctx, "establish proxy TLS", err)
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
		return nil, connectOperationError(ctx, "write proxy CONNECT request", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, connectOperationError(ctx, "read proxy CONNECT response", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy CONNECT returned HTTP status %d", response.StatusCode)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("establish proxy CONNECT: %w", err)
	}
	cancellationStopped = stopCancellation()
	if !cancellationStopped {
		return nil, fmt.Errorf("establish proxy CONNECT: %w", ctx.Err())
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear proxy CONNECT deadline: %w", err)
	}

	succeeded = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func connectDeadline(ctx context.Context, startedAt time.Time, timeout time.Duration) time.Time {
	deadline, hasDeadline := ctx.Deadline()
	if timeoutDeadline := startedAt.Add(timeout); timeout > 0 && (!hasDeadline || timeoutDeadline.Before(deadline)) {
		return timeoutDeadline
	}
	if hasDeadline {
		return deadline
	}
	return time.Time{}
}

func connectOperationError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("%s: %w", operation, contextErr)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
