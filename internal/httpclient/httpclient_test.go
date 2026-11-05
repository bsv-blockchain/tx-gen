package httpclient

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewPooledTransport(t *testing.T) {
	transport := NewPooledTransport()
	assert.Equal(t, 128, transport.MaxIdleConnsPerHost)
	assert.Equal(t, 256, transport.MaxIdleConns)
	assert.True(t, transport.ForceAttemptHTTP2)
	assert.Equal(t, 10*time.Second, transport.ResponseHeaderTimeout)
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
}

func TestNewStreamTransport(t *testing.T) {
	transport := NewStreamTransport()
	assert.Equal(t, 32, transport.MaxIdleConnsPerHost)
	assert.Zero(t, transport.ResponseHeaderTimeout, "streams may idle between events")
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
}

func TestNew(t *testing.T) {
	transport := NewPooledTransport()
	client := New(transport, 3*time.Second)
	assert.Same(t, transport, client.Transport)
	assert.Equal(t, 3*time.Second, client.Timeout)
}
