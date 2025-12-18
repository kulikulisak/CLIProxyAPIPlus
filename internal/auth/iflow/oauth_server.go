package iflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const errorRedirectURL = "https://iflow.cn/oauth/error"

// OAuthResult captures the outcome of the local OAuth callback.
type OAuthResult struct {
	Code  string
	State string
	Error string
}

// OAuthServer provides a minimal HTTP server for handling the iFlow OAuth callback.
type OAuthServer struct {
	server   *http.Server
	listener net.Listener
	port     int
	result   chan *OAuthResult
	errChan  chan error
	mu       sync.Mutex
	running  bool
}

// NewOAuthServer constructs a new OAuthServer bound to the provided port.
func NewOAuthServer(port int) *OAuthServer {
	return &OAuthServer{
		port:    port,
		result:  make(chan *OAuthResult, 1),
		errChan: make(chan error, 1),
	}
}

// Port returns the actual bound port once the server has started.
func (s *OAuthServer) Port() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Start launches the callback listener.
func (s *OAuthServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("iflow oauth server already running")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2callback", s.handleCallback)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("port %d is already in use", s.port)
		}
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	s.listener = ln
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = tcp.Port
	}

	s.running = true

	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errChan <- err
		}
	}()
	return nil
}

// Stop gracefully terminates the callback listener.
func (s *OAuthServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.server == nil {
		return nil
	}
	defer func() {
		s.running = false
		s.server = nil
	}()
	err := s.server.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	return err
}

// WaitForCallback blocks until a callback result, server error, or timeout occurs.
func (s *OAuthServer) WaitForCallback(timeout time.Duration) (*OAuthResult, error) {
	select {
	case res := <-s.result:
		return res, nil
	case err := <-s.errChan:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for OAuth callback")
	}
}

func (s *OAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	if errParam := strings.TrimSpace(query.Get("error")); errParam != "" {
		s.sendResult(&OAuthResult{Error: errParam})
		http.Redirect(w, r, errorRedirectURL, http.StatusFound)
		return
	}

	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		s.sendResult(&OAuthResult{Error: "missing_code"})
		http.Redirect(w, r, errorRedirectURL, http.StatusFound)
		return
	}

	state := query.Get("state")
	s.sendResult(&OAuthResult{Code: code, State: state})
	http.Redirect(w, r, SuccessRedirectURL, http.StatusFound)
}

func (s *OAuthServer) sendResult(res *OAuthResult) {
	select {
	case s.result <- res:
	default:
		log.Debug("iflow oauth result channel full, dropping result")
	}
}
