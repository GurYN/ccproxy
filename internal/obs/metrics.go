package obs

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics bundles every Prometheus collector ccproxy exposes. A single
// instance is shared across the server; collectors are cheap and lock-free
// to update.
type Metrics struct {
	Registry *prometheus.Registry

	RequestsTotal      *prometheus.CounterVec
	RequestDuration    *prometheus.HistogramVec
	FirstChunkLatency  *prometheus.HistogramVec
	SubprocessSpawns   prometheus.Counter
	SessionEvictions   prometheus.Counter
	ActiveSessions     prometheus.Gauge
	ActiveSubprocesses prometheus.Gauge

	// Bridge metrics (M4).
	BridgeConnections prometheus.Gauge
	BridgeRPCTotal    *prometheus.CounterVec
	BridgeRPCSeconds  *prometheus.HistogramVec
}

// NewMetrics constructs Metrics with a fresh registry. Callers expose the
// registry via promhttp.HandlerFor(metrics.Registry, ...).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		RequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "ccproxy_requests_total",
			Help: "Count of HTTP requests by endpoint, method, status, and token name.",
		}, []string{"endpoint", "method", "status", "token_name"}),
		RequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ccproxy_request_duration_seconds",
			Help:    "Wall-clock duration of an HTTP request, including subprocess work.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10), // 50ms → 25.6s
		}, []string{"endpoint"}),
		FirstChunkLatency: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ccproxy_first_chunk_latency_seconds",
			Help:    "Time from request acceptance to the first SSE data chunk.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"endpoint"}),
		SubprocessSpawns: factory.NewCounter(prometheus.CounterOpts{
			Name: "ccproxy_subprocess_spawns_total",
			Help: "Count of `claude` subprocess spawns. M3.3 warm-pool decision uses this.",
		}),
		SessionEvictions: factory.NewCounter(prometheus.CounterOpts{
			Name: "ccproxy_session_evictions_total",
			Help: "Count of persistent sessions evicted due to TTL expiry.",
		}),
		ActiveSessions: factory.NewGauge(prometheus.GaugeOpts{
			Name: "ccproxy_active_sessions",
			Help: "Number of persistent sessions currently in the registry.",
		}),
		ActiveSubprocesses: factory.NewGauge(prometheus.GaugeOpts{
			Name: "ccproxy_active_subprocesses",
			Help: "Number of `claude` subprocesses currently running.",
		}),
		BridgeConnections: factory.NewGauge(prometheus.GaugeOpts{
			Name: "ccproxy_bridge_connections",
			Help: "Number of bridge daemons currently connected.",
		}),
		BridgeRPCTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "ccproxy_bridge_rpc_total",
			Help: "Bridge RPCs issued by the server, by method and result (ok|error).",
		}, []string{"method", "result"}),
		BridgeRPCSeconds: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ccproxy_bridge_rpc_seconds",
			Help:    "Bridge RPC latency in seconds, by method.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms → ~4s
		}, []string{"method"}),
	}
}

// --- convenience wrappers --------------------------------------------------
// These exist so callers in other packages (session, chat handler) don't
// need to know the internal collector shapes, and so we can swap
// implementations (e.g. for tests) behind small interfaces.

func (m *Metrics) ActiveSessionsInc()      { m.ActiveSessions.Inc() }
func (m *Metrics) ActiveSessionsDec()      { m.ActiveSessions.Dec() }
func (m *Metrics) SessionEvicted()         { m.SessionEvictions.Inc() }
func (m *Metrics) SubprocessSpawned()      { m.SubprocessSpawns.Inc() }
func (m *Metrics) ActiveSubprocessesInc()  { m.ActiveSubprocesses.Inc() }
func (m *Metrics) ActiveSubprocessesDec()  { m.ActiveSubprocesses.Dec() }

// ObserveFirstChunk records the time from request acceptance to the first
// SSE data chunk for a given endpoint.
func (m *Metrics) ObserveFirstChunk(endpoint string, d time.Duration) {
	m.FirstChunkLatency.WithLabelValues(endpoint).Observe(d.Seconds())
}

// BridgeConnected and BridgeDisconnected adjust the active bridge gauge.
func (m *Metrics) BridgeConnected()    { m.BridgeConnections.Inc() }
func (m *Metrics) BridgeDisconnected() { m.BridgeConnections.Dec() }

// ObserveBridgeRPC records one server→daemon RPC outcome.
func (m *Metrics) ObserveBridgeRPC(method, result string, d time.Duration) {
	m.BridgeRPCTotal.WithLabelValues(method, result).Inc()
	m.BridgeRPCSeconds.WithLabelValues(method).Observe(d.Seconds())
}
