package metrics

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "tr_engine"

// HTTP metrics (counter/histogram — incremented by middleware).
var (
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "Total HTTP requests processed.",
	}, []string{"method", "path_pattern", "status_code"})

	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "path_pattern"})

	HTTPResponseSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_response_size_bytes",
		Help:      "HTTP response size in bytes.",
		Buckets:   prometheus.ExponentialBuckets(100, 10, 7), // 100B → 100MB
	}, []string{"method", "path_pattern"})
)

// Ingest counters (incremented directly by pipeline).
var (
	MQTTMessagesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "mqtt_messages_total",
		Help:      "Total MQTT messages received.",
	})

	MQTTHandlerMessagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "mqtt_handler_messages_total",
		Help:      "MQTT messages processed per handler.",
	}, []string{"handler"})

	SSEEventsPublishedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "sse_events_published_total",
		Help:      "Total SSE events published.",
	})
)

func init() {
	prometheus.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		HTTPResponseSize,
		MQTTMessagesTotal,
		MQTTHandlerMessagesTotal,
		SSEEventsPublishedTotal,
	)
}

// InstrumentHandler returns middleware that records HTTP request metrics.
// It uses chi's route pattern as the path label to avoid cardinality explosion.
func InstrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)

		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = "unknown"
		}
		method := r.Method
		status := strconv.Itoa(sw.status)
		duration := time.Since(start).Seconds()

		HTTPRequestsTotal.WithLabelValues(method, pattern, status).Inc()
		HTTPRequestDuration.WithLabelValues(method, pattern).Observe(duration)
		HTTPResponseSize.WithLabelValues(method, pattern).Observe(float64(sw.written))
	})
}

// statusWriter wraps http.ResponseWriter to capture status code and bytes written.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Flush implements http.Flusher by delegating to the wrapped writer.
// Required for SSE streaming through the metrics middleware.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker by delegating to the wrapped writer.
// Required for WebSocket upgrade through the metrics middleware.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("wrapped ResponseWriter does not implement http.Hijacker")
}

// Unwrap supports http.ResponseController and middleware that check for
// wrapped writers (e.g. http.Flusher for SSE streaming).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
