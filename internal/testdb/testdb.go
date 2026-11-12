// Package testdb gives every test its own schema inside a real PostgreSQL
// database. Tests share the server but never share tables, so they can run in
// parallel and in any order without truncation rituals.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/777Alpha777/orders-go/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const defaultURL = "postgres://postgres:postgres@localhost:5433/orders_go_test"

type DB struct {
	Store  *store.Store
	conn   *pgx.Conn
	schema string
}

func New(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	// Both the fixture connection and the pool must resolve unqualified table
	// names inside the test schema; otherwise fixtures would land in public.
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	conn, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect to %s: %v (start PostgreSQL and create the test database, see README)", dsn, err)
	}

	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	db, err := store.New(ctx, parsed.String(), 8)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(cleanupCtx)
	})

	return &DB{Store: db, conn: conn, schema: schema}
}

// Exec runs SQL in the test schema, for arranging fixtures.
func (d *DB) Exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := d.conn.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// SeedProduct inserts a product and returns its id.
func (d *DB) SeedProduct(t *testing.T, sku string, priceCents int64, stock int) int64 {
	t.Helper()

	var id int64
	err := d.conn.QueryRow(context.Background(),
		`INSERT INTO products (sku, name, price_cents, stock)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		sku, fmt.Sprintf("Product %s", sku), priceCents, stock,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return id
}

func (d *DB) Stock(t *testing.T, productID int64) int {
	t.Helper()

	var stock int
	if err := d.conn.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id = $1`, productID,
	).Scan(&stock); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return stock
}
