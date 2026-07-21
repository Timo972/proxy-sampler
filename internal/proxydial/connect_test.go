package proxydial

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type connectRequest struct {
	requestLine        string
	host               string
	proxyAuthorization string
}

func TestConnectDialerEstablishesAuthenticatedTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requests := make(chan connectRequest, 1)
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()

		request, reader, err := readConnectRequest(conn)
		if err != nil {
			serverErrors <- err
			return
		}
		requests <- request
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\npong"); err != nil {
			serverErrors <- err
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(reader, payload); err != nil {
			serverErrors <- err
			return
		}
		if string(payload) != "ping" {
			serverErrors <- fmt.Errorf("unexpected tunnel payload")
			return
		}
		serverErrors <- nil
	}()

	raw := "http://us%65r:s%65cret@" + listener.Addr().String()
	dialer, err := FromURL(raw, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "pong" {
		t.Fatalf("tunnel reply = %q", reply)
	}

	request := receive(t, requests)
	if request.requestLine != "CONNECT target.example:443 HTTP/1.1" {
		t.Fatalf("request line = %q", request.requestLine)
	}
	if request.host != "target.example:443" {
		t.Fatalf("Host = %q", request.host)
	}
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret"))
	if request.proxyAuthorization != wantAuthorization {
		t.Fatal("unexpected Proxy-Authorization header")
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSConnectDialerNegotiatesTLSWithCredentialFreeSNI(t *testing.T) {
	certificate, roots := localhostCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverNames := make(chan string, 1)
	serverErrors := make(chan error, 1)
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- hello.ServerName
			return nil, nil
		},
	}
	requests := make(chan connectRequest, 1)
	go func() {
		rawConn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		conn := tls.Server(rawConn, serverConfig)
		defer conn.Close()
		request, reader, err := readConnectRequest(conn)
		if err != nil {
			serverErrors <- err
			return
		}
		requests <- request
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\npong"); err != nil {
			serverErrors <- err
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(reader, payload); err != nil {
			serverErrors <- err
			return
		}
		if string(payload) != "ping" {
			serverErrors <- errors.New("unexpected HTTPS tunnel payload")
			return
		}
		serverErrors <- nil
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	dialer, err := FromURL(fmt.Sprintf("https://us%%65r:s%%65cret@localhost:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connectDialer, ok := dialer.(*connectDialer)
	if !ok {
		t.Fatalf("dialer type = %T", dialer)
	}
	connectDialer.tlsConfig.RootCAs = roots
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "pong" {
		t.Fatalf("tunnel reply = %q", reply)
	}

	if serverName := receive(t, serverNames); serverName != "localhost" {
		t.Fatalf("TLS ServerName = %q, want localhost", serverName)
	}
	request := receive(t, requests)
	if request.requestLine != "CONNECT target.example:443 HTTP/1.1" || request.host != "target.example:443" {
		t.Fatalf("unexpected CONNECT request over TLS")
	}
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret"))
	if request.proxyAuthorization != wantAuthorization {
		t.Fatal("unexpected HTTPS Proxy-Authorization header")
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestConnectDialerRejects407AndClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		if _, _, err := readConnectRequest(conn); err != nil {
			serverErrors <- err
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"); err != nil {
			serverErrors <- err
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			serverErrors <- err
			return
		}
		var one [1]byte
		_, err = conn.Read(one[:])
		if err != io.EOF {
			serverErrors <- fmt.Errorf("client did not close rejected tunnel")
			return
		}
		serverErrors <- nil
	}()

	dialer, err := FromURL("http://"+listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if conn != nil {
		conn.Close()
		t.Fatal("407 returned a connection")
	}
	if !strings.Contains(fmt.Sprint(err), "407") {
		t.Fatalf("err = %v", err)
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestConnectDialerCancellationInterruptsStalledResponseAndClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requestRead := make(chan struct{})
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		if _, _, err := readConnectRequest(conn); err != nil {
			serverErrors <- err
			return
		}
		close(requestRead)
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			serverErrors <- err
			return
		}
		var one [1]byte
		_, err = conn.Read(one[:])
		if err != io.EOF {
			serverErrors <- fmt.Errorf("client did not close cancelled CONNECT")
			return
		}
		serverErrors <- nil
	}()

	dialer, err := FromURL("http://"+listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialResult := make(chan error, 1)
	go func() {
		conn, err := dialer.DialContext(ctx, "tcp", "target.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		dialResult <- err
	}()

	receive(t, requestRead)
	cancel()
	select {
	case err := <-dialResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context cancellation", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("DialContext did not return promptly after cancellation")
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestConnectDialerTimeoutInterruptsStalledResponseAndClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		if _, _, err := readConnectRequest(conn); err != nil {
			serverErrors <- err
			return
		}
		serverErrors <- expectPeerClosed(conn)
	}()

	dialer, err := FromURL("http://"+listener.Addr().String(), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("stalled proxy returned a connection")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("timeout returned after %v", elapsed)
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestConnectDialerClearsHandshakeDeadlineBeforeReturningTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		_, reader, err := readConnectRequest(conn)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			serverErrors <- err
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(reader, payload); err != nil {
			serverErrors <- err
			return
		}
		_, err = conn.Write(payload)
		serverErrors <- err
	}()

	dialer, err := FromURL("http://"+listener.Addr().String(), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(75 * time.Millisecond)
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "ping" {
		t.Fatalf("tunnel reply = %q", reply)
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestConnectDialerClosesConnectionAfterMalformedResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		if _, _, err := readConnectRequest(conn); err != nil {
			serverErrors <- err
			return
		}
		if _, err := io.WriteString(conn, "not an HTTP response\r\n\r\n"); err != nil {
			serverErrors <- err
			return
		}
		serverErrors <- expectPeerClosed(conn)
	}()

	dialer, err := FromURL("http://"+listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("malformed response returned a connection")
	}
	if err == nil {
		t.Fatal("malformed response was accepted")
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSConnectDialerClosesConnectionAfterTLSHandshakeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			serverErrors <- err
			return
		}
		payload := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
		if _, err := io.ReadFull(conn, payload); err != nil {
			serverErrors <- err
			return
		}
		if _, err := conn.Write([]byte{21, 3, 3, 0, 2, 2, 40}); err != nil {
			serverErrors <- err
			return
		}
		serverErrors <- expectPeerClosed(conn)
	}()

	dialer, err := FromURL("https://"+listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("failed TLS handshake returned a connection")
	}
	if err == nil {
		t.Fatal("TLS handshake failure was accepted")
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSConnectDialerClosesRejectedTunnel(t *testing.T) {
	certificate, roots := localhostCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErrors := make(chan error, 1)
	go func() {
		rawConn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		conn := tls.Server(rawConn, &tls.Config{Certificates: []tls.Certificate{certificate}})
		defer conn.Close()
		if _, _, err := readConnectRequest(conn); err != nil {
			serverErrors <- err
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"); err != nil {
			serverErrors <- err
			return
		}
		serverErrors <- expectPeerClosed(conn)
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	dialer, err := FromURL(fmt.Sprintf("https://localhost:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dialer.(*connectDialer).tlsConfig.RootCAs = roots
	conn, err := dialer.DialContext(context.Background(), "tcp", "target.example:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("407 returned a connection")
	}
	if !strings.Contains(fmt.Sprint(err), "407") {
		t.Fatalf("err = %v", err)
	}
	if err := receive(t, serverErrors); err != nil {
		t.Fatal(err)
	}
}

func readConnectRequest(conn net.Conn) (connectRequest, *bufio.Reader, error) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return connectRequest{}, nil, err
	}
	headers, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		return connectRequest{}, nil, err
	}
	return connectRequest{
		requestLine:        strings.TrimRight(line, "\r\n"),
		host:               headers.Get("Host"),
		proxyAuthorization: headers.Get("Proxy-Authorization"),
	}, reader, nil
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test server")
		var zero T
		return zero
	}
}

func localhostCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}, roots
}

func expectPeerClosed(conn net.Conn) error {
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fmt.Errorf("client did not close connection: %w", err)
	}
	return nil
}
