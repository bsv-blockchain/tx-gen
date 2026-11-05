// Package metrics defines the Prometheus metrics that tx-gen exports on
// /metrics.
//
// Every method is safe to call on a nil *Metrics, which records nothing. This
// lets tests and optional components skip metrics without guarding each call.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// unknownLabel replaces an empty label value.
const unknownLabel = "unknown"

// bootstrapStages lists every bootstrap stage the engine reports. All of them
// are reset when the stage changes, so that alert rules such as
// txgen_bootstrap_stage{stage="l2_done"} != 1 always have a series to test.
var bootstrapStages = [...]string{"none", "l1_done", "l2_partial", "l2_done", "unknown"}

// Metrics holds the tx-gen collectors and the registry that serves them.
type Metrics struct {
	registry *prometheus.Registry

	broadcastTotal       *prometheus.CounterVec
	broadcastLatency     *prometheus.HistogramVec
	queueDepth           prometheus.Gauge
	chainsActive         prometheus.Gauge
	tpsTarget            prometheus.Gauge
	sseConnected         *prometheus.GaugeVec
	sseEventTotal        *prometheus.CounterVec
	sseDisconnectTotal   *prometheus.CounterVec
	reorgTotal           *prometheus.CounterVec
	bootstrapStage       *prometheus.GaugeVec
	panicTotal           *prometheus.CounterVec
	authFailTotal        prometheus.Counter
	chainTerminatedTotal *prometheus.CounterVec
}

// New creates the tx-gen metrics on a dedicated registry, which also carries
// the standard Go runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		broadcastTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_broadcast_total", Help: "Total broadcasts by kind and result"},
			[]string{"kind", "result"},
		),
		broadcastLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "txgen_broadcast_latency_seconds", Help: "Broadcast latency", Buckets: prometheus.DefBuckets},
			[]string{"kind"},
		),
		queueDepth:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_queue_depth", Help: "Current queue depth"}),
		chainsActive: prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_chains_active", Help: "Active chains"}),
		tpsTarget:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_tps_target", Help: "Target TPS"}),
		sseConnected: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "txgen_sse_connected", Help: "SSE connected"},
			[]string{"stream"},
		),
		sseEventTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_sse_event_total", Help: "SSE events by stream"},
			[]string{"stream"},
		),
		sseDisconnectTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_sse_disconnect_total", Help: "SSE disconnects by stream"},
			[]string{"stream"},
		),
		reorgTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_reorg_total", Help: "Reorg events by depth"},
			[]string{"depth"},
		),
		bootstrapStage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "txgen_bootstrap_stage", Help: "Bootstrap stage"},
			[]string{"stage"},
		),
		panicTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_panic_total", Help: "Panics by goroutine"},
			[]string{"goroutine"},
		),
		authFailTotal: prometheus.NewCounter(prometheus.CounterOpts{Name: "txgen_auth_fail_total", Help: "Auth failures"}),
		chainTerminatedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "txgen_chain_terminated_total", Help: "Chains terminated"},
			[]string{"reason"},
		),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.broadcastTotal,
		m.broadcastLatency,
		m.queueDepth,
		m.chainsActive,
		m.tpsTarget,
		m.sseConnected,
		m.sseEventTotal,
		m.sseDisconnectTotal,
		m.reorgTotal,
		m.bootstrapStage,
		m.panicTotal,
		m.authFailTotal,
		m.chainTerminatedTotal,
	)
	return m
}

// Handler serves the registry in the Prometheus exposition format. A nil
// *Metrics serves 404 Not Found.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Registry returns the registry holding every tx-gen collector, or nil for a
// nil *Metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// IncBroadcast counts one broadcast attempt of kind ending in result
// ("ok", "retry", or "error").
func (m *Metrics) IncBroadcast(kind, result string) {
	if m == nil {
		return
	}
	m.broadcastTotal.WithLabelValues(kind, result).Inc()
}

// ObserveBroadcastLatency records how long one broadcast request of kind took.
func (m *Metrics) ObserveBroadcastLatency(kind string, d time.Duration) {
	if m == nil {
		return
	}
	m.broadcastLatency.WithLabelValues(kind).Observe(d.Seconds())
}

// SetChainsActive sets the number of chains currently running.
func (m *Metrics) SetChainsActive(n int) {
	if m == nil {
		return
	}
	m.chainsActive.Set(float64(n))
}

// SetTPSTarget sets the requested transactions per second.
func (m *Metrics) SetTPSTarget(n int64) {
	if m == nil {
		return
	}
	m.tpsTarget.Set(float64(n))
}

// SetQueueDepth sets the number of UTXOs waiting in the queue.
func (m *Metrics) SetQueueDepth(n int) {
	if m == nil {
		return
	}
	m.queueDepth.Set(float64(n))
}

// SetSSEConnected records whether the named SSE stream is connected.
func (m *Metrics) SetSSEConnected(connected bool, stream string) {
	if m == nil {
		return
	}
	value := 0.0
	if connected {
		value = 1
	}
	m.sseConnected.WithLabelValues(labelOrUnknown(stream)).Set(value)
}

// IncSSEEvent counts one connection or event on the named SSE stream.
func (m *Metrics) IncSSEEvent(stream string) {
	if m == nil {
		return
	}
	m.sseEventTotal.WithLabelValues(labelOrUnknown(stream)).Inc()
}

// IncSSEDisconnect counts one disconnect of the named SSE stream.
func (m *Metrics) IncSSEDisconnect(stream string) {
	if m == nil {
		return
	}
	m.sseDisconnectTotal.WithLabelValues(labelOrUnknown(stream)).Inc()
}

// IncReorg counts one chain reorganization of the given depth.
func (m *Metrics) IncReorg(depth int) {
	if m == nil {
		return
	}
	m.reorgTotal.WithLabelValues(strconv.Itoa(depth)).Inc()
}

// IncAuthFail counts one rejected admin request.
func (m *Metrics) IncAuthFail() {
	if m == nil {
		return
	}
	m.authFailTotal.Inc()
}

// IncChainTerminated counts one chain that stopped for reason.
func (m *Metrics) IncChainTerminated(reason string) {
	if m == nil {
		return
	}
	m.chainTerminatedTotal.WithLabelValues(labelOrUnknown(reason)).Inc()
}

// SetBootstrapStage marks stage as the current bootstrap stage (1) and every
// other known stage as inactive (0).
func (m *Metrics) SetBootstrapStage(stage string) {
	if m == nil {
		return
	}
	for _, known := range bootstrapStages {
		m.bootstrapStage.WithLabelValues(known).Set(0)
	}
	m.bootstrapStage.WithLabelValues(labelOrUnknown(stage)).Set(1)
}

// IncPanic counts one recovered panic in the named goroutine.
func (m *Metrics) IncPanic(goroutine string) {
	if m == nil {
		return
	}
	m.panicTotal.WithLabelValues(goroutine).Inc()
}

// labelOrUnknown returns value, or "unknown" when value is empty.
func labelOrUnknown(value string) string {
	if value == "" {
		return unknownLabel
	}
	return value
}
