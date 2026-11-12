// Package metrics holds the Prometheus collectors and the middleware that
// records HTTP traffic. A dedicated registry is used instead of the default one
// so tests can build an API without global state leaking between them.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	registry *prometheus.Registry

	ordersCreated *prometheus.CounterVec
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
}

func New() *Metrics {
	registry := prometheus.NewRegistry()

	ordersCreated := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orders_created_total",
		Help: "Order creation attempts by outcome",
	}, []string{"outcome"})

	httpRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests by method, route and status",
	}, []string{"method", "route", "status"})

	httpDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"method", "route"})

	registry.MustRegister(ordersCreated, httpRequests, httpDuration)

	return &Metrics{
		registry:      registry,
		ordersCreated: ordersCreated,
		httpRequests:  httpRequests,
		httpDuration:  httpDuration,
	}
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// OrderOutcome records how a checkout ended: created, replayed or conflict.
func (m *Metrics) OrderOutcome(outcome string) {
	m.ordersCreated.WithLabelValues(outcome).Inc()
}

// Middleware records status and latency. The route pattern is used, not the
// raw path, otherwise every /orders/<uuid> would create its own time series.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		route := "unmatched"
		if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
			route = pattern
		}
		m.httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(recorder.status)).Inc()
		m.httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(started).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
