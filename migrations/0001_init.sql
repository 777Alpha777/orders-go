-- Initial schema: catalogue, orders, order items and idempotency keys.
--
-- Money is stored in cents as BIGINT: floating point has no place in prices.
-- Stock has a CHECK (stock >= 0) constraint on purpose: if a bug ever lets an
-- oversell through the locking path, the database refuses the write instead of
-- silently going negative.

CREATE TABLE products (
    id          BIGSERIAL PRIMARY KEY,
    sku         TEXT   NOT NULL UNIQUE,
    name        TEXT   NOT NULL,
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    stock       INTEGER NOT NULL CHECK (stock >= 0)
);

CREATE TABLE orders (
    id          UUID PRIMARY KEY,
    customer_id TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('pending', 'paid', 'cancelled')),
    total_cents BIGINT NOT NULL CHECK (total_cents >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The feed query is always "latest orders in this status": the composite index
-- matches that access path exactly, and cmd/bench measures the difference.
CREATE INDEX ix_orders_status_created_at ON orders (status, created_at DESC);

CREATE TABLE order_items (
    order_id         UUID   NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    product_id       BIGINT NOT NULL REFERENCES products (id),
    quantity         INTEGER NOT NULL CHECK (quantity > 0),
    unit_price_cents BIGINT NOT NULL CHECK (unit_price_cents >= 0),
    PRIMARY KEY (order_id, product_id)
);

-- A row is inserted before the order is created, inside the same transaction.
-- That makes the key itself the lock: two concurrent requests with one key
-- cannot both create an order, the second one waits and then replays the body.
CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    body         JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
