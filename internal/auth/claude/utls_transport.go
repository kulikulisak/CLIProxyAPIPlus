// Package claude provides authentication functionality for Anthropic's Claude API.
// This file implements a custom HTTP transport using utls to bypass TLS fingerprinting.
package claude

import (
	"net"
	"net/http"
	"strings"
	"sync"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// utlsRoundTripper implements http.RoundTripper using utls with Firefox fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	// mu protects the connections map
	mu sync.Mutex
	// connections caches HTTP/2 client connections per host
	connections map[string]*http2.ClientConn
}

// newUtlsRoundTripper creates a new utls-based round tripper
func newUtlsRoundTripper() *utlsRoundTripper {
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
	}
}

// RoundTrip implements http.RoundTripper
func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	// Try to reuse existing connection
	t.mu.Lock()
	h2Conn, ok := t.connections[host]
	t.mu.Unlock()

	if ok && h2Conn.CanTakeNewRequest() {
		resp, err := h2Conn.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		// Connection failed, remove it and create new one
		t.mu.Lock()
		delete(t.connections, host)
		t.mu.Unlock()
	}

	// Create new TLS connection with Firefox fingerprint
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: req.URL.Hostname()}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloFirefox_Auto)

	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	// Create HTTP/2 client connection
	tr := &http2.Transport{}
	h2Conn, err = tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	// Cache the connection
	t.mu.Lock()
	t.connections[host] = h2Conn
	t.mu.Unlock()

	return h2Conn.RoundTrip(req)
}

// NewAnthropicHttpClient creates an HTTP client that bypasses TLS fingerprinting
// for Anthropic domains by using utls with Firefox fingerprint.
func NewAnthropicHttpClient() *http.Client {
	return &http.Client{
		Transport: newUtlsRoundTripper(),
	}
}
