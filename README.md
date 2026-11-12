# orders-go

Order service in Go: HTTP JSON API, PostgreSQL, stock reservation and idempotent checkout.
It is the Go counterpart of my Python [orders-service](https://github.com/777Alpha777/orders-service),
rewritten with different transport and storage decisions rather than translated line by line.

The interesting part is not the CRUD. It is what happens when twenty customers buy the last
item at the same time, or when a client retries a checkout because the network died after the
server had already committed.

## Stack

| Layer | Choice | Why |
|---|---|---|
| HTTP | `chi` + stdlib handlers | tiny router, `http.Handler` all the way down, no framework |
| Storage | `pgx/v5` + `pgxpool` | real PostgreSQL protocol, no ORM hiding the SQL |
| Migrations | embedded SQL, applied at start under an advisory lock | one binary, several replicas, no migration container |
| Metrics | Prometheus, own registry | tests can build an API without global collectors |
| Logs | `log/slog` JSON | structured, no logging framework |

## Measured

`cmd/bench` seeds 200 000 orders, runs the feed query 60 times (`LIMIT 20`, `WHERE status = 'paid'`)
and reads the planner's buffer counters, so the result does not depend only on how fast the machine is.

| Index | p50 | Shared buffers |
|---|---|---|
| none | 37.94 ms | 2062 |
| `(status, created_at DESC)` | 0.50 ms | 4 |

Windows 11, PostgreSQL 17.11, local SSD. Reproduce with `make bench` (or `go run ./cmd/bench`).
The speedup is expected from the plan shape and is what the composite index exists for; the
absolute milliseconds on your machine will differ.

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/orders` | create an order, `Idempotency-Key` header required |
| `GET` | `/api/v1/orders?status=&limit=&offset=` | orders newest first with items and total count |
| `GET` | `/api/v1/products` | catalogue with current stock |
| `GET` | `/healthz` | liveness |
| `GET` | `/metrics` | Prometheus exposition |

```bash
curl -s -X POST localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 4f8c2f8e-2c1a-4a0e-9b7e-1f0b7f3a2c11' \
  -d '{"customer_id":"customer-1","items":[{"product_id":1,"quantity":2}]}'

# retry the same request: 200 instead of 201, same order id, stock reserved once
curl -si -X POST localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 4f8c2f8e-2c1a-4a0e-9b7e-1f0b7f3a2c11' \
  -d '{"customer_id":"customer-1","items":[{"product_id":1,"quantity":2}]}'
```

Errors are uniform:

```json
{"error":{"type":"insufficient_stock","message":"insufficient stock for product 3: requested 1, available 0",
          "details":{"product_id":3,"requested":1,"available":0}}}
```

## Checkout

```mermaid
sequenceDiagram
    participant C as Client
    participant A as API
    participant P as PostgreSQL

    C->>A: POST /api/v1/orders + Idempotency-Key
    A->>P: BEGIN
    A->>P: INSERT idempotency_keys ... ON CONFLICT DO NOTHING
    alt key already exists
        P-->>A: no row returned
        A->>P: SELECT request_hash, body
        A-->>C: 200 with the stored order
    else key claimed
        A->>P: SELECT products WHERE id = ANY(...) ORDER BY id FOR UPDATE
        A->>P: UPDATE stock, INSERT orders, INSERT order_items
        A->>P: UPDATE idempotency_keys SET body
        A->>P: COMMIT
        A-->>C: 201 Created
    end
```

## Design decisions

- **Stock is reserved with row locks, taken in a fixed order.** `SELECT ... ORDER BY id FOR UPDATE`
  makes concurrent checkouts queue behind each other in the same order, which is why two carts
  with products `{1,2}` and `{2,1}` cannot deadlock. This is a checkout, not a read-heavy feed:
  a short serialisation per product row is the price of never overselling.
- **`CHECK (stock >= 0)` is a second line of defence.** If a future code path forgets the lock,
  the database refuses the write instead of going negative. The API turns the constraint error
  into a 500 and logs it, because reaching that point is a bug, not a business conflict.
- **Idempotency is the insert itself.** The key row is inserted first, inside the same
  transaction as the order. A concurrent request with the same key blocks on the primary key,
  then sees the stored `request_hash` and `body` and replays the response. There is no
  "check then insert" race and no separate lock table.
- **The payload hash distinguishes retries from key reuse.** Same key + same body = replay (200).
  Same key + different body = 409, because returning the old order would hide a client bug.
- **Money is `BIGINT` cents.** No floats anywhere near prices.
- **The list endpoint is two queries, not N+1.** Orders are loaded, then their items are fetched
  with one `WHERE order_id = ANY($1)` and stitched in memory.
- **The feed index matches the query exactly.** `(status, created_at DESC)` turns a 200k-row
  sort into an index scan, which is what the table above measures.

## Tests

Integration tests run against real PostgreSQL, no mocks: the point of the store is the locking
behaviour, and a mock would test nothing.

```bash
createdb -p 5433 -U postgres orders_go_test
go test ./...
```

Every test gets its own schema (`test_<random>`) with `search_path` set on the pool, so tests
are independent and can run in parallel without cleaning shared tables. The concurrency test
fires 20 buyers at one remaining unit and asserts exactly one order was created, 19 received
`insufficient_stock`, and stock ended at zero.

The API tests use `httptest.Server` against the real store, so JSON decoding, status codes and
the replay header are covered end to end.

## Running locally

```bash
docker compose up --build
curl -s localhost:8080/healthz
```

Without Docker:

```bash
export DATABASE_URL='postgres://postgres:postgres@localhost:5433/orders_go'
go run ./cmd/migrate
go run ./cmd/seed
go run ./cmd/api
```

Configuration is environment only: `HTTP_ADDR`, `DATABASE_URL`, `DB_MAX_CONNS`, `SHUTDOWN_TIMEOUT`.

## Layout

```
cmd/api        HTTP server, migrations at start, graceful shutdown
cmd/migrate    schema step only, for pipelines
cmd/seed       idempotent demo catalogue
cmd/bench      index benchmark with planner buffers
internal/api   transport: routes, validation, error mapping, middleware
internal/store transactions, locking, SQL
internal/metrics Prometheus counters and HTTP middleware
internal/testdb  per-test PostgreSQL schema
migrations     embedded SQL
```

## Limitations

This is a portfolio service, not a production order platform. It has no authentication, no
payment state machine beyond `pending/paid/cancelled`, no outbox and no rate limiting. Only the
`POST /orders` path is idempotent; `GET` endpoints are safe by definition. Metrics cover the HTTP
layer and checked-out outcomes, not pool saturation.
