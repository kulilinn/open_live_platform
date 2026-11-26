package observability

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	Discovery prometheus.Gauge
	Reloads   *prometheus.CounterVec
}

func New() *Metrics {
	result := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "Completed HTTP requests by route."}, []string{"path", "method"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_response_time_seconds", Help: "HTTP response time by route.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 60}}, []string{"path", "method", "status"}),
		Discovery: prometheus.NewGauge(prometheus.GaugeOpts{Name: "platform_etcd_discovery_registered", Help: "Whether the gRPC endpoint is registered in etcd."}),
		Reloads:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "platform_etcd_config_reload_total", Help: "Configuration updates accepted or rejected."}, []string{"result"}),
	}
	result.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		result.requests, result.latency, result.Discovery, result.Reloads)
	return result
}

func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	return mux
}

func (m *Metrics) Observe(next http.Handler, routes *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &response{ResponseWriter: w, status: http.StatusOK, observe: func(code int) {
			pattern := r.Pattern
			if pattern == "" {
				_, pattern = routes.Handler(r)
			}
			if _, path, ok := strings.Cut(pattern, " "); ok {
				pattern = path
			}
			if pattern == "" {
				pattern = "unmatched"
			}
			method := r.Method
			switch method {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
			default:
				method = "other"
			}
			m.requests.WithLabelValues(pattern, method).Inc()
			m.latency.WithLabelValues(pattern, method, strconv.Itoa(code)).Observe(time.Since(start).Seconds())
		}}
		defer recorder.finish()
		next.ServeHTTP(recorder, r)
	})
}

type response struct {
	http.ResponseWriter
	status                int
	wroteHeader, recorded bool
	observe               func(int)
}

func (r *response) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *response) WriteHeader(code int) {
	if !r.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *response) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

func (r *response) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buff, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
		r.finish()
	}
	return conn, buff, err
}

func (r *response) finish() {
	if !r.recorded {
		r.recorded = true
		r.observe(r.status)
	}
}
