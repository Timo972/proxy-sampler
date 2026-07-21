package proxydial

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
