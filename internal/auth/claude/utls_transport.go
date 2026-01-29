// Package claude provides authentication functionality for Anthropic's Claude API.
// This file implements a custom HTTP transport using utls to bypass TLS fingerprinting.
package claude

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Firefox fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	// mu protects the connections map
	mu sync.Mutex
	// connections caches HTTP/2 client connections per host
	connections map[string]*http2.ClientConn
	// proxyURL is the optional proxy URL from configuration
	proxyURL string
}

// newUtlsRoundTripper creates a new utls-based round tripper with optional proxy support
func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		proxyURL:    proxyURL,
	}
}

// dial creates a TCP connection, optionally through a proxy
func (t *utlsRoundTripper) dial(addr string) (net.Conn, error) {
	if t.proxyURL == "" {
		return net.Dial("tcp", addr)
	}

	proxyParsed, err := url.Parse(t.proxyURL)
	if err != nil {
		// Fall back to direct connection if proxy URL is invalid
		return net.Dial("tcp", addr)
	}

	if proxyParsed.Scheme == "socks5" {
		var proxyAuth *proxy.Auth
		if proxyParsed.User != nil {
			username := proxyParsed.User.Username()
			password, _ := proxyParsed.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", proxyParsed.Host, proxyAuth, proxy.Direct)
		if err != nil {
			return net.Dial("tcp", addr)
		}
		return dialer.Dial("tcp", addr)
	}

	// For HTTP/HTTPS proxies, use direct connection (proxy handled at HTTP level)
	// Note: HTTP proxies with CONNECT would require more complex handling
	return net.Dial("tcp", addr)
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
	conn, err := t.dial(addr)
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
// It accepts optional SDK configuration for proxy settings.
func NewAnthropicHttpClient(cfg *config.SDKConfig) *http.Client {
	proxyURL := ""
	if cfg != nil {
		proxyURL = cfg.ProxyURL
	}
	return &http.Client{
		Transport: newUtlsRoundTripper(proxyURL),
	}
}
