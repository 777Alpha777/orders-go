package store

import (
	"context"
	"fmt"
)

type Product struct {
	ID         int64  `json:"id"`
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
	Stock      int    `json:"stock"`
}

func (s *Store) ListProducts(ctx context.Context, limit int) ([]Product, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, sku, name, price_cents, stock FROM products ORDER BY id LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select products: %w", err)
	}
	defer rows.Close()

	products := make([]Product, 0, limit)
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name, &p.PriceCents, &p.Stock); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		products = append(products, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read products: %w", err)
	}
	return products, nil
}
