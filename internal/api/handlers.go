package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/777Alpha777/orders-go/internal/store"
)

const (
	maxBodyBytes   = 1 << 20 // 1 MiB
	maxKeyLength   = 128
	maxListLimit   = 100
	defaultListLim = 20
)

type createOrderRequest struct {
	CustomerID string                 `json:"customer_id"`
	Items      []store.OrderItemInput `json:"items"`
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > maxKeyLength {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key",
			fmt.Sprintf("Idempotency-Key header is required and must be at most %d characters", maxKeyLength))
		return
	}

	var req createOrderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := validateCreateOrder(req); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	order, replayed, err := a.store.CreateOrder(r.Context(), store.CreateOrderInput{
		CustomerID:     req.CustomerID,
		IdempotencyKey: key,
		Items:          req.Items,
	})
	if err != nil {
		a.handleCreateError(w, r, err)
		return
	}

	if replayed {
		a.metrics.OrderOutcome("replayed")
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, order)
		return
	}

	a.metrics.OrderOutcome("created")
	writeJSON(w, http.StatusCreated, order)
}

func (a *API) handleCreateError(w http.ResponseWriter, r *http.Request, err error) {
	var stockErr *store.InsufficientStockError
	switch {
	case errors.As(err, &stockErr):
		a.metrics.OrderOutcome("conflict")
		writeJSON(w, http.StatusConflict, errorBody{
			Error: errorDetail{
				Type:    "insufficient_stock",
				Message: stockErr.Error(),
				Details: map[string]any{
					"product_id": stockErr.ProductID,
					"requested":  stockErr.Requested,
					"available":  stockErr.Available,
				},
			},
		})
	case errors.Is(err, store.ErrKeyReuse):
		a.metrics.OrderOutcome("conflict")
		writeError(w, http.StatusConflict, "idempotency_key_reused",
			"this Idempotency-Key was already used with a different payload")
	case errors.Is(err, store.ErrRequestInProgress):
		a.metrics.OrderOutcome("conflict")
		writeError(w, http.StatusConflict, "request_in_progress",
			"another request with this Idempotency-Key is in progress, retry shortly")
	case errors.Is(err, store.ErrProductNotFound):
		a.metrics.OrderOutcome("conflict")
		writeError(w, http.StatusNotFound, "product_not_found", "one of the requested products does not exist")
	default:
		a.metrics.OrderOutcome("error")
		a.log.ErrorContext(r.Context(), "create order failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", store.StatusPending, store.StatusPaid, store.StatusCancelled:
	default:
		writeError(w, http.StatusBadRequest, "invalid_status",
			"status must be one of: pending, paid, cancelled")
		return
	}

	limit, err := intParam(r, "limit", defaultListLim)
	if err != nil || limit < 1 || limit > maxListLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit",
			fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
		return
	}

	offset, err := intParam(r, "offset", 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be zero or positive")
		return
	}

	orders, total, err := a.store.List(r.Context(), status, limit, offset)
	if err != nil {
		a.log.ErrorContext(r.Context(), "list orders failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":  orders,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := a.store.ListProducts(r.Context(), maxListLimit)
	if err != nil {
		a.log.ErrorContext(r.Context(), "list products failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": products})
}

func validateCreateOrder(req createOrderRequest) error {
	if strings.TrimSpace(req.CustomerID) == "" {
		return errors.New("customer_id is required")
	}
	if len(req.Items) == 0 {
		return errors.New("items must not be empty")
	}
	for _, item := range req.Items {
		if item.ProductID <= 0 {
			return errors.New("items[].product_id must be positive")
		}
		if item.Quantity <= 0 {
			return errors.New("items[].quantity must be positive")
		}
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

func intParam(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

type errorDetail struct {
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Type: kind, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent: an encode error cannot be reported to
	// the client any more and would only duplicate a 200 with a broken body.
	_ = json.NewEncoder(w).Encode(payload)
}
