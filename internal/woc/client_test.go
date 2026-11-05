package woc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const testHash = "6032c38c0bc0e91e726f1e55e1832e434509001a7aed5cfd881b6ef07215e84a"

var errTransport = errors.New("connection refused")

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewDefaults(t *testing.T) {
	c := New(Options{})
	assert.Equal(t, DefaultBaseURL, c.baseURL)
	assert.Equal(t, 1, c.retryMax)
	assert.Equal(t, defaultTimeout, c.http.Timeout)
}

func TestFetchUTXOs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/bsv/main/script/"+testHash+"/unspent/all", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"result":[{"height":900000,"tx_hash":"aa","tx_pos":1,"value":5000},`+
			`{"height":0,"tx_hash":"bb","tx_pos":0,"value":7}]}`)
	}))
	defer server.Close()

	c := New(Options{BaseURL: server.URL + "/v1/bsv/main/"})
	utxos, err := c.FetchUTXOs(t.Context(), testHash)
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{
		{TxHash: "aa", TxPos: 1, Value: 5000, Height: 900000},
		{TxHash: "bb", TxPos: 0, Value: 7, Height: 0},
	}, utxos)
}

func TestFetchUTXOsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":[]}`)
	}))
	defer server.Close()
	utxos, err := New(Options{BaseURL: server.URL}).FetchUTXOs(t.Context(), testHash)
	require.NoError(t, err)
	assert.Empty(t, utxos)
}

func TestFetchUTXOsRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, `{"result":[{"tx_hash":"aa","tx_pos":0,"value":1}]}`)
	}))
	defer server.Close()

	utxos, err := New(Options{BaseURL: server.URL, RetryMax: 3}).FetchUTXOs(t.Context(), testHash)
	require.NoError(t, err)
	assert.Len(t, utxos, 1)
	assert.Equal(t, int32(3), calls.Load())
}

func TestFetchUTXOsDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, strings.Repeat("x", maxErrorBytes+100))
	}))
	defer server.Close()

	_, err := New(Options{BaseURL: server.URL, RetryMax: 3}).FetchUTXOs(t.Context(), testHash)
	require.ErrorIs(t, err, ErrRequestFailed)
	assert.Contains(t, err.Error(), "HTTP 404")
	assert.Less(t, len(err.Error()), maxErrorBytes+100, "error body is truncated")
	assert.Equal(t, int32(1), calls.Load())
}

func TestFetchUTXOsReportsAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":[],"error":"script hash not found"}`)
	}))
	defer server.Close()
	_, err := New(Options{BaseURL: server.URL}).FetchUTXOs(t.Context(), testHash)
	require.ErrorIs(t, err, ErrRequestFailed)
	assert.Contains(t, err.Error(), "script hash not found")
}

func TestFetchUTXOsRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[not json`)
	}))
	defer server.Close()
	_, err := New(Options{BaseURL: server.URL, RetryMax: 3}).FetchUTXOs(t.Context(), testHash)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

func TestFetchUTXOsRetriesTransportErrors(t *testing.T) {
	var calls atomic.Int32
	c := New(Options{RetryMax: 2, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errTransport
	})}})
	_, err := c.FetchUTXOs(t.Context(), testHash)
	require.ErrorIs(t, err, errTransport)
	assert.Equal(t, int32(2), calls.Load())
}

func TestFetchUTXOsStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := New(Options{RetryMax: 5, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})}})
	_, err := c.FetchUTXOs(ctx, testHash)
	require.ErrorIs(t, err, context.Canceled)
}
