// Package api is the HTTP transport layer. It knows about status codes and
// JSON, and nothing about transactions or SQL: that belongs to internal/store.
package api

import (
	"log/slog"
	"net/http"

	"github.com/777Alpha777/orders-go/internal/metrics"
	"github.com/777Alpha777/orders-go/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type API struct {
	store   *store.Store
	log     *slog.Logger
	metrics *metrics.Metrics
}

func New(st *store.Store, log *slog.Logger, m *metrics.Metrics) *API {
	return &API{store: st, log: log, metrics: m}
}

func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(a.metrics.Middleware)
	r.Use(a.recoverer)
	r.Use(a.logRequests)

	r.Get("/healthz", a.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/orders", a.createOrder)
		r.Get("/orders", a.listOrders)
		r.Get("/products", a.listProducts)
	})

	return r
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
