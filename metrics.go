package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	broadcastTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "txgen_broadcast_total", Help: "Total broadcasts by kind and result"},
		[]string{"kind", "result"},
	)
	broadcastLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "txgen_broadcast_latency_seconds", Help: "Broadcast latency", Buckets: prometheus.DefBuckets},
		[]string{"kind"},
	)
	queueDepth         = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_queue_depth", Help: "Current queue depth"})
	chainsActive       = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_chains_active", Help: "Active chains"})
	tpsTarget          = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_tps_target", Help: "Target TPS"})
	sseConnected       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "txgen_sse_connected", Help: "SSE connected"}, []string{"stream"})
	sseEventTotal      = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "txgen_sse_event_total", Help: "SSE events by stream"}, []string{"stream"})
	sseDisconnectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "txgen_sse_disconnect_total", Help: "SSE disconnects by stream"}, []string{"stream"})
	reorgTotal         = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "txgen_reorg_total", Help: "Reorg events by depth"},
		[]string{"depth"},
	)
	bootstrapStage       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "txgen_bootstrap_stage", Help: "Bootstrap stage"}, []string{"stage"})
	panicTotal           = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "txgen_panic_total", Help: "Panics by goroutine"}, []string{"goroutine"})
	authFailTotal        = prometheus.NewCounter(prometheus.CounterOpts{Name: "txgen_auth_fail_total", Help: "Auth failures"})
	chainTerminatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "txgen_chain_terminated_total", Help: "Chains terminated"}, []string{"reason"})
)

func init() {
	prometheus.MustRegister(broadcastTotal, broadcastLatency, queueDepth, chainsActive, tpsTarget, sseConnected, sseEventTotal, sseDisconnectTotal, reorgTotal, bootstrapStage, panicTotal, authFailTotal, chainTerminatedTotal)
}

func RegisterMetrics(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.Handler())
}

func IncBroadcast(kind, result string) {
	broadcastTotal.WithLabelValues(kind, result).Inc()
}

func ObserveBroadcastLatency(kind string, d time.Duration) {
	broadcastLatency.WithLabelValues(kind).Observe(d.Seconds())
}

func SetChainsActive(n int) {
	chainsActive.Set(float64(n))
}

func SetTPSTarget(n int) {
	tpsTarget.Set(float64(n))
}

func SetQueueDepth(n int) {
	queueDepth.Set(float64(n))
}

func SetSSEConnected(b bool, stream string) {
	if stream == "" {
		stream = "unknown"
	}
	if b {
		sseConnected.WithLabelValues(stream).Set(1)
	} else {
		sseConnected.WithLabelValues(stream).Set(0)
	}
}

func IncSSEEvent(stream string) {
	if stream == "" {
		stream = "unknown"
	}
	sseEventTotal.WithLabelValues(stream).Inc()
}

func IncSSEDisconnect(stream string) {
	if stream == "" {
		stream = "unknown"
	}
	sseDisconnectTotal.WithLabelValues(stream).Inc()
}

func IncReorg(depth int) {
	reorgTotal.WithLabelValues(fmt.Sprintf("%d", depth)).Inc()
}

func IncAuthFail() {
	authFailTotal.Inc()
}

func IncChainTerminated(reason string) {
	if reason == "" {
		reason = "unknown"
	}
	chainTerminatedTotal.WithLabelValues(reason).Inc()
}

func SetBootstrapStage(stage string) {
	bootstrapStage.WithLabelValues(stage).Set(1)
}

func IncPanic(goroutine string) {
	panicTotal.WithLabelValues(goroutine).Inc()
}
