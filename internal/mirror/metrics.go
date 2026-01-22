package mirror

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	registry       *prometheus.Registry
	requests       *prometheus.CounterVec
	requestBytes   *prometheus.CounterVec
	responseBytes  *prometheus.CounterVec
	upstreamErrors *prometheus.CounterVec
	fallbacks      *prometheus.CounterVec
	inflight       prometheus.Gauge
	duration       *prometheus.HistogramVec
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rmirror_requests_total",
				Help: "Total HTTP requests handled.",
			},
			[]string{"method", "route", "status"},
		),
		requestBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rmirror_request_bytes_total",
				Help: "Total request bytes received.",
			},
			[]string{"route"},
		),
		responseBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rmirror_response_bytes_total",
				Help: "Total response bytes sent.",
			},
			[]string{"route"},
		),
		upstreamErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rmirror_upstream_errors_total",
				Help: "Total upstream errors.",
			},
			[]string{"route"},
		),
		fallbacks: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rmirror_tls_fallback_total",
				Help: "Total TLS fragment fallback attempts.",
			},
			[]string{"from", "to"},
		),
		inflight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "rmirror_inflight_requests",
				Help: "Current inflight requests.",
			},
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rmirror_request_duration_seconds",
				Help:    "Request duration in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "route"},
		),
	}
	m.registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		m.requests,
		m.requestBytes,
		m.responseBytes,
		m.upstreamErrors,
		m.fallbacks,
		m.inflight,
		m.duration,
	)
	return m
}

func (m *metrics) observeRequest(route, method string, status int, duration time.Duration, reqBytes, respBytes int64) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	if reqBytes > 0 {
		m.requestBytes.WithLabelValues(route).Add(float64(reqBytes))
	}
	if respBytes > 0 {
		m.responseBytes.WithLabelValues(route).Add(float64(respBytes))
	}
	m.duration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *metrics) observeUpstreamError(route string) {
	if m == nil {
		return
	}
	m.upstreamErrors.WithLabelValues(route).Inc()
}

func (m *metrics) observeFallback(from, to uint8) {
	if m == nil {
		return
	}
	m.fallbacks.WithLabelValues(strconv.Itoa(int(from)), strconv.Itoa(int(to))).Inc()
}
