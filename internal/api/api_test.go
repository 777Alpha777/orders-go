package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/777Alpha777/orders-go/internal/api"
	"github.com/777Alpha777/orders-go/internal/metrics"
	"github.com/777Alpha777/orders-go/internal/testdb"
)

func newServer(t *testing.T) (*testdb.DB, *httptest.Server) {
	t.Helper()

	db := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(api.New(db.Store, logger, metrics.New()).Router())
	t.Cleanup(server.Close)

	return db, server
}

func postOrder(t *testing.T, server *httptest.Server, key string, body any) *http.Response {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/orders", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

type orderResponse struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	TotalCents int64  `json:"total_cents"`
	Status     string `json:"status"`
	Items      []struct {
		ProductID int64 `json:"product_id"`
		Quantity  int   `json:"quantity"`
	} `json:"items"`
}

type errorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Details struct {
			ProductID int64 `json:"product_id"`
			Requested int   `json:"requested"`
			Available int   `json:"available"`
		} `json:"details"`
	} `json:"error"`
}

func TestCreateOrder(t *testing.T) {
	db, server := newServer(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 129900, 10)

	resp := postOrder(t, server, "key-1", map[string]any{
		"customer_id": "customer-1",
		"items":       []map[string]any{{"product_id": product, "quantity": 2}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var order orderResponse
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if order.TotalCents != 259800 {
		t.Errorf("total = %d, want 259800", order.TotalCents)
	}
	if order.Status != "pending" {
		t.Errorf("status = %q, want pending", order.Status)
	}
}

func TestReplayReturnsStoredOrderWith200(t *testing.T) {
	db, server := newServer(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 1000, 10)

	body := map[string]any{
		"customer_id": "customer-1",
		"items":       []map[string]any{{"product_id": product, "quantity": 1}},
	}

	first := postOrder(t, server, "key-1", body)
	var firstOrder orderResponse
	_ = json.NewDecoder(first.Body).Decode(&firstOrder)
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}

	second := postOrder(t, server, "key-1", body)
	defer second.Body.Close()

	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.StatusCode)
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Error("Idempotent-Replayed header is missing")
	}

	var secondOrder orderResponse
	if err := json.NewDecoder(second.Body).Decode(&secondOrder); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if secondOrder.ID != firstOrder.ID {
		t.Errorf("replay id = %s, want %s", secondOrder.ID, firstOrder.ID)
	}

	if got := db.Stock(t, product); got != 9 {
		t.Errorf("stock = %d, want 9", got)
	}
}

func TestInsufficientStockReturns409(t *testing.T) {
	db, server := newServer(t)
	product := db.SeedProduct(t, "SKU-MUG", 39900, 1)

	postOrder(t, server, "key-1", map[string]any{
		"customer_id": "customer-1",
		"items":       []map[string]any{{"product_id": product, "quantity": 1}},
	}).Body.Close()

	resp := postOrder(t, server, "key-2", map[string]any{
		"customer_id": "customer-2",
		"items":       []map[string]any{{"product_id": product, "quantity": 1}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}

	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Type != "insufficient_stock" {
		t.Errorf("type = %q, want insufficient_stock", body.Error.Type)
	}
	if body.Error.Details.Available != 0 || body.Error.Details.Requested != 1 {
		t.Errorf("details = %+v, want available 0 / requested 1", body.Error.Details)
	}
}

func TestValidationErrors(t *testing.T) {
	_, server := newServer(t)

	tests := []struct {
		name string
		key  string
		body map[string]any
	}{
		{"no idempotency key", "", map[string]any{
			"customer_id": "c", "items": []map[string]any{{"product_id": 1, "quantity": 1}},
		}},
		{"empty items", "key-1", map[string]any{"customer_id": "c", "items": []map[string]any{}}},
		{"zero quantity", "key-2", map[string]any{
			"customer_id": "c", "items": []map[string]any{{"product_id": 1, "quantity": 0}},
		}},
		{"no customer", "key-3", map[string]any{
			"customer_id": " ", "items": []map[string]any{{"product_id": 1, "quantity": 1}},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := postOrder(t, server, tc.key, tc.body)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestListOrders(t *testing.T) {
	db, server := newServer(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 1000, 100)

	for _, key := range []string{"key-1", "key-2", "key-3"} {
		postOrder(t, server, key, map[string]any{
			"customer_id": "customer-1",
			"items":       []map[string]any{{"product_id": product, "quantity": 1}},
		}).Body.Close()
	}

	resp, err := server.Client().Get(server.URL + "/api/v1/orders?status=pending&limit=2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Items  []orderResponse `json:"items"`
		Total  int             `json:"total"`
		Limit  int             `json:"limit"`
		Offset int             `json:"offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Total != 3 {
		t.Errorf("total = %d, want 3", body.Total)
	}
	if len(body.Items) != 2 {
		t.Errorf("items = %d, want 2", len(body.Items))
	}
}

func TestHealth(t *testing.T) {
	_, server := newServer(t)

	resp, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
