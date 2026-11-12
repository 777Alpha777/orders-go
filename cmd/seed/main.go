// Command seed fills the catalogue with a handful of products. It is
// idempotent: running it twice does not duplicate rows.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/777Alpha777/orders-go/internal/config"
	"github.com/777Alpha777/orders-go/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type product struct {
	sku   string
	name  string
	price int64
	stock int
}

var catalogue = []product{
	{"SKU-COFFEE-1", "Coffee, 1 kg", 129900, 50},
	{"SKU-TEA-1", "Tea, 500 g", 64900, 120},
	{"SKU-MUG-1", "Ceramic mug", 39900, 3},
	{"SKU-BEANS-1", "Coffee beans, 250 g", 45900, 0},
	{"SKU-GRINDER-1", "Burr grinder", 249900, 7},
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := config.Load()

	db, err := store.New(ctx, cfg.DatabaseURL, cfg.MaxPoolConns)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	for _, p := range catalogue {
		if _, err := pool.Exec(ctx,
			`INSERT INTO products (sku, name, price_cents, stock)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (sku) DO NOTHING`,
			p.sku, p.name, p.price, p.stock,
		); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("seeded %d products\n", len(catalogue))
}
