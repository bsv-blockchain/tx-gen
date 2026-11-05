// Package httpclient builds the tuned HTTP transports and clients used to
// talk to Arcade, WhatsOnChain, Slack, and the long-lived SSE streams.
package httpclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// NewPooledTransport returns a transport sized for high-concurrency
// request/response traffic such as broadcasts and UTXO lookups.
//
// The defaults of http.DefaultTransport (two idle connections per host) would
// force a new TLS handshake for almost every broadcast at thousands of chains.
func NewPooledTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		MaxIdleConnsPerHost:   128,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewStreamTransport returns a transport for long-lived SSE connections. It
// sets no response-header timeout, because a stream can idle between events.
func NewStreamTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		MaxIdleConnsPerHost: 32,
		MaxIdleConns:        64,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// New returns a client that sends requests through transport. A zero timeout
// disables the overall per-request timeout, as SSE streams require.
func New(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport, Timeout: timeout}
}
