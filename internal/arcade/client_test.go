package arcade

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/metrics"
)

const testTxID = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"

var errTransport = errors.New("connection refused")

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// newTestClient returns a client whose requests are answered by fn.
func newTestClient(retryMax int, m *metrics.Metrics, fn roundTripFunc) *Client {
	return New(Options{
		BaseURL:       "http://arcade.test/",
		CallbackToken: "callback-secret",
		RetryMax:      retryMax,
		HTTPClient:    &http.Client{Transport: fn},
		Metrics:       m,
	})
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{})
	assert.Equal(t, DefaultBaseURL, c.baseURL)
	assert.Equal(t, 1, c.retryMax)
	assert.Equal(t, defaultConcurrency, cap(c.slots))
	assert.Equal(t, defaultTimeout, c.http.Timeout)
}

func TestBroadcastAccepted(t *testing.T) {
	m := metrics.New()
	c := newTestClient(3, m, func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/tx", r.URL.Path)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Equal(t, "callback-secret", r.Header.Get("X-CallbackToken"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, []byte{1, 2, 3}, body)
		return response(http.StatusAccepted, `{"txStatus":"SEEN_ON_NETWORK"}`), nil
	})

	require.NoError(t, c.Broadcast(t.Context(), testTxID, []byte{1, 2, 3}))
	assert.Equal(t, 1, testutil.CollectAndCount(m.Registry(), "txgen_broadcast_latency_seconds"))
}

func TestBroadcastOmitsEmptyCallbackToken(t *testing.T) {
	c := New(Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, set := r.Header["X-Callbacktoken"]
		assert.False(t, set)
		return response(http.StatusAccepted, ""), nil
	})}})
	require.NoError(t, c.Broadcast(t.Context(), "", nil))
}

// failingBody returns some bytes and then a read error.
type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errTransport
	}
	b.sent = true
	return copy(p, `{"txStatus":"SEEN`), nil
}

func (b *failingBody) Close() error { return nil }

func TestBroadcastAcceptedDespiteBodyReadError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(3, nil, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusAccepted, Body: &failingBody{}}, nil
	})
	require.NoError(t, c.Broadcast(t.Context(), testTxID, []byte{1}))
	assert.Equal(t, int32(1), calls.Load(), "an accepted broadcast is never sent again")
}

func TestBroadcastRetriesRetryableStatuses(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(3, nil, func(*http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return response(http.StatusTooManyRequests, "slow down"), nil
		case 2:
			return response(http.StatusBadGateway, "upstream"), nil
		default:
			return response(http.StatusAccepted, ""), nil
		}
	})
	require.NoError(t, c.Broadcast(t.Context(), testTxID, []byte{1}))
	assert.Equal(t, int32(3), calls.Load())
}

func TestBroadcastRetriesTransportErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(2, nil, func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errTransport
		}
		return response(http.StatusAccepted, ""), nil
	})
	require.NoError(t, c.Broadcast(t.Context(), testTxID, []byte{1}))
	assert.Equal(t, int32(2), calls.Load())
}

func TestBroadcastFailsFastOnClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(3, nil, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusBadRequest, `{"status":"REJECTED"}`), nil
	})
	err := c.Broadcast(t.Context(), testTxID, []byte{1})
	require.ErrorIs(t, err, ErrRejected)
	assert.Contains(t, err.Error(), "HTTP 400")
	assert.Equal(t, int32(1), calls.Load())
}

func TestBroadcastGivesUpAfterRetryMax(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(2, nil, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusServiceUnavailable, "down"), nil
	})
	require.ErrorIs(t, c.Broadcast(t.Context(), testTxID, []byte{1}), ErrRejected)
	assert.Equal(t, int32(2), calls.Load())
}

func TestBroadcastStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := newTestClient(5, nil, func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})
	require.ErrorIs(t, c.Broadcast(ctx, testTxID, []byte{1}), context.Canceled)
}

func TestBroadcastWaitsForSlot(t *testing.T) {
	c := New(Options{Concurrency: 1, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusAccepted, ""), nil
	})}})
	c.slots <- struct{}{} // occupy the only slot

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.Broadcast(ctx, testTxID, nil), context.DeadlineExceeded)
}

func TestTxStatus(t *testing.T) {
	c := newTestClient(1, nil, func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/tx/"+testTxID, r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		return response(http.StatusOK, `{"txStatus":"MINED"}`), nil
	})
	status, body, err := c.TxStatus(t.Context(), testTxID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{"txStatus":"MINED"}`, body)
}

func TestTxStatusTransportError(t *testing.T) {
	c := newTestClient(1, nil, func(*http.Request) (*http.Response, error) { return nil, errTransport })
	_, _, err := c.TxStatus(t.Context(), testTxID)
	require.ErrorIs(t, err, errTransport)
}

func TestReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	require.NoError(t, New(Options{BaseURL: server.URL}).Reachable(t.Context()))

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	require.ErrorIs(t, New(Options{BaseURL: failing.URL}).Reachable(t.Context()), ErrUnhealthy)

	unreachable := newTestClient(1, nil, func(*http.Request) (*http.Response, error) { return nil, errTransport })
	require.ErrorIs(t, unreachable.Reachable(t.Context()), errTransport)
}

func TestTxStatusFromBody(t *testing.T) {
	assert.Equal(t, "MINED", TxStatusFromBody(`{"txStatus":"MINED"}`))
	assert.Equal(t, "SEEN", TxStatusFromBody(`{"tx_status":"SEEN"}`))
	assert.Equal(t, "submitted", TxStatusFromBody(`{"status":"submitted"}`))
	assert.Empty(t, TxStatusFromBody(`{"status":7}`))
	assert.Empty(t, TxStatusFromBody("not json"))
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", Truncate("abc", 3))
	assert.Equal(t, "ab...[truncated]", Truncate("abc", 2))
	assert.Equal(t, "abc", Truncate("abc", 0))
}
