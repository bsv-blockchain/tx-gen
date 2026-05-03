package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

func ArcadeTransport() *http.Transport {
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

func SSETransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		MaxIdleConnsPerHost:   32,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
	}
}

func NewHTTPClient(transport *http.Transport, timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport, Timeout: timeout}
}
