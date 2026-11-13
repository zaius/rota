package proxy

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// A proxy may pipeline the target's first bytes (a TLS ServerHello) immediately
// after the CONNECT response. Those bytes belong to the tunnel and must still be
// readable from the connection afterwards.
func TestReadCONNECTResponse_DoesNotConsumeTunnelBytes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	tunnelBytes := []byte{0x16, 0x03, 0x01, 0x02, 0x00} // TLS handshake record header
	go func() {
		// A single write: the proxy pipelines the tunnel's first bytes into the
		// same segment as its response. A bulk read would swallow them.
		pipelined := append([]byte("HTTP/1.1 200 Connection established\r\nProxy-Agent: test\r\n\r\n"), tunnelBytes...)
		server.Write(pipelined)
	}()

	line, err := readCONNECTResponse(client)
	if err != nil {
		t.Fatalf("readCONNECTResponse: %v", err)
	}
	if line != "HTTP/1.1 200 Connection established" {
		t.Fatalf("unexpected status line %q", line)
	}

	got := make([]byte, len(tunnelBytes))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("reading pipelined tunnel bytes: %v", err)
	}
	if !bytes.Equal(got, tunnelBytes) {
		t.Fatalf("tunnel bytes were consumed by the response reader: got %v want %v", got, tunnelBytes)
	}
}

// Headers arriving in several packets must be reassembled, not truncated.
func TestReadCONNECTResponse_HandlesSplitWrites(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		server.Write([]byte("HTTP/1.1 200 Conn"))
		server.Write([]byte("ection established\r\n"))
		server.Write([]byte("X-Test: 1\r\n\r\n"))
	}()

	line, err := readCONNECTResponse(client)
	if err != nil {
		t.Fatalf("readCONNECTResponse: %v", err)
	}
	if line != "HTTP/1.1 200 Connection established" {
		t.Fatalf("unexpected status line %q", line)
	}
}
