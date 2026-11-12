// Command bench measures the orders feed query with and without the composite
// index. It prints p50 latency and the number of shared buffers touched, which
// is the part that does not depend on the machine's speed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	seedRows = 200_000
	runs     = 60
	warmup   = 3
)

const feedQuery = `
	SELECT id, customer_id, status, total_cents, created_at
	FROM orders
	WHERE status = 'paid'
	ORDER BY created_at DESC
	LIMIT 20`

const indexDDL = `CREATE INDEX ix_orders_status_created_at ON orders (status, created_at DESC)`

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5433/orders_go"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fail("connect: %v", err)
	}
	defer pool.Close()

	if err := seed(ctx, pool); err != nil {
		fail("seed: %v", err)
	}

	withIndex, err := measure(ctx, pool)
	if err != nil {
		fail("measure with index: %v", err)
	}

	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS ix_orders_status_created_at`); err != nil {
		fail("drop index: %v", err)
	}

	withoutIndex, err := measure(ctx, pool)
	if err != nil {
		fail("measure without index: %v", err)
	}

	if _, err := pool.Exec(ctx, indexDDL); err != nil {
		fail("recreate index: %v", err)
	}

	fmt.Printf("\n%d orders, %d runs, LIMIT 20, status = 'paid'\n\n", seedRows, runs)
	fmt.Println("| Index | p50 | buffers |")
	fmt.Println("|---|---|---|")
	fmt.Printf("| none | %.2f ms | %d |\n", withoutIndex.p50Ms, withoutIndex.buffers)
	fmt.Printf("| (status, created_at DESC) | %.2f ms | %d |\n", withIndex.p50Ms, withIndex.buffers)
	fmt.Printf("\nspeedup: x%.1f\n", withoutIndex.p50Ms/withIndex.p50Ms)
}

type result struct {
	p50Ms   float64
	buffers int64
}

func seed(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `TRUNCATE orders CASCADE`); err != nil {
		return err
	}

	started := time.Now()
	_, err := pool.Exec(ctx, `
		INSERT INTO orders (id, customer_id, status, total_cents, created_at)
		SELECT gen_random_uuid(),
		       'customer-' || (i % 1000),
		       (ARRAY['pending', 'paid', 'cancelled'])[1 + (i % 3)],
		       (i % 50000)::bigint,
		       now() - (i || ' seconds')::interval
		FROM generate_series(1, $1) AS i`, seedRows)
	if err != nil {
		return err
	}

	fmt.Printf("seeded %d orders in %s\n", seedRows, time.Since(started).Round(time.Millisecond))
	return nil
}

func measure(ctx context.Context, pool *pgxpool.Pool) (result, error) {
	for range warmup {
		if _, err := pool.Exec(ctx, feedQuery); err != nil {
			return result{}, err
		}
	}

	durations := make([]time.Duration, 0, runs)
	for range runs {
		started := time.Now()
		rows, err := pool.Query(ctx, feedQuery)
		if err != nil {
			return result{}, err
		}
		for rows.Next() {
			// Scan into throwaway values: the benchmark measures the query, not
			// the decoding.
			var (
				id         uuid.UUID
				customerID string
				status     string
				total      int64
				createdAt  time.Time
			)
			if err := rows.Scan(&id, &customerID, &status, &total, &createdAt); err != nil {
				rows.Close()
				return result{}, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result{}, err
		}
		durations = append(durations, time.Since(started))
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	buffers, err := sharedBuffers(ctx, pool)
	if err != nil {
		return result{}, err
	}

	return result{
		p50Ms:   float64(durations[len(durations)/2].Microseconds()) / 1000.0,
		buffers: buffers,
	}, nil
}

// sharedBuffers sums the buffers the planner reports for the query. Reading the
// plan is more honest than inferring index usage from timing alone.
func sharedBuffers(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var raw []byte
	if err := pool.QueryRow(ctx,
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+feedQuery,
	).Scan(&raw); err != nil {
		return 0, err
	}

	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return 0, err
	}
	if len(plan) == 0 {
		return 0, nil
	}

	var total int64
	for _, key := range []string{"Shared Hit Blocks", "Shared Read Blocks"} {
		if value, ok := plan[0].Plan[key].(float64); ok {
			total += int64(value)
		}
	}
	return total, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
