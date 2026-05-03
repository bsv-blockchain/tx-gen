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
	queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_queue_depth", Help: "Current queue depth"})
	chainsActive = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_chains_active", Help: "Active chains"})
	tpsTarget = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_tps_target", Help: "Target TPS"})
	sseConnected = prometheus.NewGauge(prometheus.GaugeOpts{Name: "txgen_sse_connected", Help: "SSE connected"})
	reorgTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "txgen_reorg_total", Help: "Reorg events by depth"},
		[]string{"depth"},
	)
	bootstrapStage = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "txgen_bootstrap_stage", Help: "Bootstrap stage"}, []string{"stage"})
	panicTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "txgen_panic_total", Help: "Panics by goroutine"}, []string{"goroutine"})
)

func init() {
	prometheus.MustRegister(broadcastTotal, broadcastLatency, queueDepth, chainsActive, tpsTarget, sseConnected, reorgTotal, bootstrapStage, panicTotal)
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

func SetSSEConnected(b bool) {
	if b {
		sseConnected.Set(1)
	} else {
		sseConnected.Set(0)
	}
}

func IncReorg(depth int) {
	reorgTotal.WithLabelValues(fmt.Sprintf("%d", depth)).Inc()
}

func SetBootstrapStage(stage string) {
	bootstrapStage.WithLabelValues(stage).Set(1)
}

func IncPanic(goroutine string) {
	panicTotal.WithLabelValues(goroutine).Inc()
}
