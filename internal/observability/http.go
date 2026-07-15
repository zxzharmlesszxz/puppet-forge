package observability

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"puppet-forge/internal/metrics"
)

type Middleware struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	panicsTotal     *prometheus.CounterVec
	inFlight        prometheus.Gauge
}

func NewMiddleware() *Middleware {
	return &Middleware{
		requestsTotal: metrics.RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "puppet_forge_http_requests_total",
			Help: "Total number of HTTP requests processed by the service.",
		}, []string{"method", "route", "status"})),
		requestDuration: metrics.RegisterHistogramVec(prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "puppet_forge_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status"})),
		panicsTotal: metrics.RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "puppet_forge_http_panics_total",
			Help: "Total number of recovered HTTP handler panics.",
		}, []string{"method", "route"})),
		inFlight: metrics.RegisterGauge(prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "puppet_forge_http_in_flight_requests",
			Help: "Current number of in-flight HTTP requests.",
		})),
	}
}

func (m *Middleware) MetricsHandler() http.Handler {
	return promhttp.Handler()
}

func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inFlight.Inc()
		route := classifyRoute(r.URL.Path)
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			m.inFlight.Dec()
			if recovered := recover(); recovered != nil {
				m.panicsTotal.WithLabelValues(r.Method, route).Inc()
				if !rec.headerWritten {
					http.Error(rec, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
				slog.Default().Error("http handler panic", "panic", recovered, "method", r.Method, "path", r.URL.Path, "route", route)
				if !rec.headerWritten {
					rec.status = http.StatusInternalServerError
				}
			}

			status := strconv.Itoa(rec.status)
			duration := time.Since(start).Seconds()

			m.requestsTotal.WithLabelValues(r.Method, route, status).Inc()
			m.requestDuration.WithLabelValues(r.Method, route, status).Observe(duration)

			slog.Default().Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", route,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", time.Since(start).Round(time.Millisecond),
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		}()

		next.ServeHTTP(rec, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status        int
	bytes         int
	headerWritten bool
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.headerWritten {
		return
	}
	r.status = statusCode
	r.headerWritten = true
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.headerWritten = true
	n, err := r.ResponseWriter.Write(body)
	r.bytes += n
	return n, err
}

func classifyRoute(path string) string {
	switch {
	case path == "/":
		return "/"
	case path == "/healthz":
		return "/healthz"
	case path == "/readyz":
		return "/readyz"
	case path == "/metrics":
		return "/metrics"
	case path == "/manage":
		return "/manage"
	case len(path) >= len("/manage/") && path[:len("/manage/")] == "/manage/":
		return "/manage/*"
	case path == "/api/v1/modules":
		return "/api/v1/modules"
	case path == "/api/v1/modules/":
		return "/api/v1/modules/"
	case len(path) >= len("/api/v1/modules/") && path[:len("/api/v1/modules/")] == "/api/v1/modules/":
		return "/api/v1/modules/*"
	case len(path) >= len("/modules/") && path[:len("/modules/")] == "/modules/":
		return "/modules/*"
	case len(path) >= len("/v3/files/") && path[:len("/v3/files/")] == "/v3/files/":
		return "/v3/files/*"
	case len(path) >= len("/v3/") && path[:len("/v3/")] == "/v3/":
		return "/v3/*"
	default:
		return "other"
	}
}
