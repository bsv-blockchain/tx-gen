package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsRecordValues(t *testing.T) {
	m := New()

	m.IncBroadcast("tx", "ok")
	m.IncBroadcast("tx", "ok")
	m.ObserveBroadcastLatency("tx", 20*time.Millisecond)
	m.SetChainsActive(12)
	m.SetTPSTarget(16)
	m.SetQueueDepth(3)
	m.SetSSEConnected(true, "tip")
	m.SetSSEConnected(false, "")
	m.IncSSEEvent("tip")
	m.IncSSEDisconnect("")
	m.IncReorg(2)
	m.IncAuthFail()
	m.IncChainTerminated("")
	m.IncPanic("chain")

	assert.InDelta(t, 2, testutil.ToFloat64(m.broadcastTotal.WithLabelValues("tx", "ok")), 0)
	assert.Equal(t, 1, testutil.CollectAndCount(m.broadcastLatency))
	assert.InDelta(t, 12, testutil.ToFloat64(m.chainsActive), 0)
	assert.InDelta(t, 16, testutil.ToFloat64(m.tpsTarget), 0)
	assert.InDelta(t, 3, testutil.ToFloat64(m.queueDepth), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.sseConnected.WithLabelValues("tip")), 0)
	assert.InDelta(t, 0, testutil.ToFloat64(m.sseConnected.WithLabelValues(unknownLabel)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.sseEventTotal.WithLabelValues("tip")), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.sseDisconnectTotal.WithLabelValues(unknownLabel)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.reorgTotal.WithLabelValues("2")), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.authFailTotal), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.chainTerminatedTotal.WithLabelValues(unknownLabel)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.panicTotal.WithLabelValues("chain")), 0)
}

func TestSetBootstrapStageResetsKnownStages(t *testing.T) {
	m := New()
	m.SetBootstrapStage("l1_done")
	m.SetBootstrapStage("l2_done")

	for _, stage := range bootstrapStages {
		want := 0.0
		if stage == "l2_done" {
			want = 1
		}
		assert.InDelta(t, want, testutil.ToFloat64(m.bootstrapStage.WithLabelValues(stage)), 0, "stage %s", stage)
	}
}

func TestHandlerServesTxGenMetrics(t *testing.T) {
	m := New()
	m.SetTPSTarget(5)
	require.NotNil(t, m.Registry())

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "txgen_tps_target 5")
	assert.Contains(t, string(body), "go_goroutines")
}

func TestNilMetricsIsNoOp(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() {
		m.IncBroadcast("tx", "ok")
		m.ObserveBroadcastLatency("tx", time.Second)
		m.SetChainsActive(1)
		m.SetTPSTarget(1)
		m.SetQueueDepth(1)
		m.SetSSEConnected(true, "tip")
		m.IncSSEEvent("tip")
		m.IncSSEDisconnect("tip")
		m.IncReorg(1)
		m.IncAuthFail()
		m.IncChainTerminated("exhausted")
		m.SetBootstrapStage("l2_done")
		m.IncPanic("chain")
	})
	assert.Nil(t, m.Registry())

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}
