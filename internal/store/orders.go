package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	StatusPending   = "pending"
	StatusPaid      = "paid"
	StatusCancelled = "cancelled"
)

var (
	// ErrKeyReuse means the same Idempotency-Key arrived with a different body.
	// Replaying the stored response would be wrong, so it is a conflict.
	ErrKeyReuse = errors.New("idempotency key reused with a different payload")
	// ErrRequestInProgress means another transaction holds the key right now.
	ErrRequestInProgress = errors.New("a request with this idempotency key is in progress")
	ErrProductNotFound   = errors.New("product not found")
)

// InsufficientStockError carries the numbers so the API can explain the 409.
type InsufficientStockError struct {
	ProductID int64
	Requested int
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf(
		"insufficient stock for product %d: requested %d, available %d",
		e.ProductID, e.Requested, e.Available,
	)
}

type OrderItemInput struct {
	ProductID int64 `json:"product_id"`
	Quantity  int   `json:"quantity"`
}

type CreateOrderInput struct {
	CustomerID     string
	IdempotencyKey string
	Items          []OrderItemInput
}

type OrderItem struct {
	ProductID      int64 `json:"product_id"`
	Quantity       int   `json:"quantity"`
	UnitPriceCents int64 `json:"unit_price_cents"`
}

type Order struct {
	ID         uuid.UUID   `json:"id"`
	CustomerID string      `json:"customer_id"`
	Status     string      `json:"status"`
	TotalCents int64       `json:"total_cents"`
	CreatedAt  time.Time   `json:"created_at"`
	Items      []OrderItem `json:"items"`
}

// CreateOrder reserves stock and creates the order atomically.
//
// The second return value is true when the request was a replay of an earlier
// one with the same Idempotency-Key: the API then answers 200 instead of 201.
func (s *Store) CreateOrder(
	ctx context.Context, in CreateOrderInput,
) (Order, bool, error) {
	items := mergeItems(in.Items)
	hash, err := payloadHash(in.CustomerID, items)
	if err != nil {
		return Order{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Claim the key first. ON CONFLICT DO NOTHING waits for a concurrent
	// transaction that already claimed it, so this is the mutual exclusion.
	var claimed string
	err = tx.QueryRow(ctx,
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, $2)
		 ON CONFLICT (key) DO NOTHING
		 RETURNING key`, in.IdempotencyKey, hash,
	).Scan(&claimed)

	if errors.Is(err, pgx.ErrNoRows) {
		replayed, err := replayOrder(ctx, tx, in.IdempotencyKey, hash)
		if err != nil {
			return Order{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Order{}, false, fmt.Errorf("commit replay: %w", err)
		}
		return replayed, true, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("claim idempotency key: %w", err)
	}

	order, err := createOrder(ctx, tx, in.CustomerID, items)
	if err != nil {
		return Order{}, false, err
	}

	body, err := json.Marshal(order)
	if err != nil {
		return Order{}, false, fmt.Errorf("encode response: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE idempotency_keys SET body = $2 WHERE key = $1`, in.IdempotencyKey, body,
	); err != nil {
		return Order{}, false, fmt.Errorf("store response: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("commit: %w", err)
	}
	return order, false, nil
}

func replayOrder(
	ctx context.Context, tx pgx.Tx, key, hash string,
) (Order, error) {
	var storedHash string
	var body []byte
	if err := tx.QueryRow(ctx,
		`SELECT request_hash, body FROM idempotency_keys WHERE key = $1`, key,
	).Scan(&storedHash, &body); err != nil {
		return Order{}, fmt.Errorf("load idempotency key: %w", err)
	}

	if storedHash != hash {
		return Order{}, ErrKeyReuse
	}
	if body == nil {
		return Order{}, ErrRequestInProgress
	}

	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return Order{}, fmt.Errorf("decode stored response: %w", err)
	}
	return order, nil
}

func createOrder(
	ctx context.Context, tx pgx.Tx, customerID string, items []OrderItemInput,
) (Order, error) {
	ids := make([]int64, len(items))
	for i, item := range items {
		ids[i] = item.ProductID
	}

	// ORDER BY id makes every transaction take row locks in the same order,
	// which is what keeps two concurrent checkouts from deadlocking.
	rows, err := tx.Query(ctx,
		`SELECT id, price_cents, stock FROM products WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
		ids,
	)
	if err != nil {
		return Order{}, fmt.Errorf("lock products: %w", err)
	}

	type product struct {
		price int64
		stock int
	}
	found := make(map[int64]product, len(items))
	for rows.Next() {
		var id int64
		var p product
		if err := rows.Scan(&id, &p.price, &p.stock); err != nil {
			rows.Close()
			return Order{}, fmt.Errorf("scan product: %w", err)
		}
		found[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Order{}, fmt.Errorf("read products: %w", err)
	}

	order := Order{
		ID:         uuid.New(),
		CustomerID: customerID,
		Status:     StatusPending,
		Items:      make([]OrderItem, 0, len(items)),
	}

	for _, item := range items {
		p, ok := found[item.ProductID]
		if !ok {
			return Order{}, ErrProductNotFound
		}
		if p.stock < item.Quantity {
			return Order{}, &InsufficientStockError{
				ProductID: item.ProductID,
				Requested: item.Quantity,
				Available: p.stock,
			}
		}
		order.TotalCents += p.price * int64(item.Quantity)
		order.Items = append(order.Items, OrderItem{
			ProductID:      item.ProductID,
			Quantity:       item.Quantity,
			UnitPriceCents: p.price,
		})
	}

	for _, item := range items {
		if _, err := tx.Exec(ctx,
			`UPDATE products SET stock = stock - $2 WHERE id = $1`, item.ProductID, item.Quantity,
		); err != nil {
			return Order{}, fmt.Errorf("reserve stock: %w", err)
		}
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO orders (id, customer_id, status, total_cents)
		 VALUES ($1, $2, $3, $4)
		 RETURNING created_at`,
		order.ID, order.CustomerID, order.Status, order.TotalCents,
	).Scan(&order.CreatedAt); err != nil {
		return Order{}, fmt.Errorf("insert order: %w", err)
	}

	for _, item := range order.Items {
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_items (order_id, product_id, quantity, unit_price_cents)
			 VALUES ($1, $2, $3, $4)`,
			order.ID, item.ProductID, item.Quantity, item.UnitPriceCents,
		); err != nil {
			return Order{}, fmt.Errorf("insert order item: %w", err)
		}
	}

	return order, nil
}

// List returns orders newest first, optionally filtered by status, together
// with the total number of matching rows for pagination.
func (s *Store) List(
	ctx context.Context, status string, limit, offset int,
) ([]Order, int, error) {
	where := ""
	args := []any{}
	if status != "" {
		where = " WHERE status = $1"
		args = append(args, status)
	}

	var total int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM orders"+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count orders: %w", err)
	}

	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx,
		"SELECT id, customer_id, status, total_cents, created_at FROM orders"+where+
			fmt.Sprintf(" ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d", len(args)-1, len(args)),
		args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("select orders: %w", err)
	}
	defer rows.Close()

	orders := make([]Order, 0, limit)
	index := map[uuid.UUID]int{}
	for rows.Next() {
		var order Order
		if err := rows.Scan(
			&order.ID, &order.CustomerID, &order.Status, &order.TotalCents, &order.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan order: %w", err)
		}
		index[order.ID] = len(orders)
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read orders: %w", err)
	}

	if len(orders) == 0 {
		return orders, total, nil
	}

	ids := make([]uuid.UUID, len(orders))
	for i, order := range orders {
		ids[i] = order.ID
	}

	itemRows, err := s.pool.Query(ctx,
		`SELECT order_id, product_id, quantity, unit_price_cents
		 FROM order_items WHERE order_id = ANY($1) ORDER BY product_id`, ids,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("select order items: %w", err)
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var orderID uuid.UUID
		var item OrderItem
		if err := itemRows.Scan(&orderID, &item.ProductID, &item.Quantity, &item.UnitPriceCents); err != nil {
			return nil, 0, fmt.Errorf("scan order item: %w", err)
		}
		if position, ok := index[orderID]; ok {
			orders[position].Items = append(orders[position].Items, item)
		}
	}
	if err := itemRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read order items: %w", err)
	}

	return orders, total, nil
}

// mergeItems collapses duplicate product ids so a body with the same product
// twice does not violate the order_items primary key.
func mergeItems(items []OrderItemInput) []OrderItemInput {
	merged := make(map[int64]int, len(items))
	for _, item := range items {
		merged[item.ProductID] += item.Quantity
	}

	out := make([]OrderItemInput, 0, len(merged))
	for productID, quantity := range merged {
		out = append(out, OrderItemInput{ProductID: productID, Quantity: quantity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProductID < out[j].ProductID })
	return out
}

func payloadHash(customerID string, items []OrderItemInput) (string, error) {
	payload, err := json.Marshal(struct {
		CustomerID string           `json:"customer_id"`
		Items      []OrderItemInput `json:"items"`
	}{CustomerID: customerID, Items: items})
	if err != nil {
		return "", fmt.Errorf("hash payload: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
