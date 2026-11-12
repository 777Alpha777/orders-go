package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/777Alpha777/orders-go/internal/store"
	"github.com/777Alpha777/orders-go/internal/testdb"
)

func TestCreateOrderReservesStock(t *testing.T) {
	db := testdb.New(t)
	coffee := db.SeedProduct(t, "SKU-COFFEE", 129900, 10)
	mug := db.SeedProduct(t, "SKU-MUG", 39900, 5)

	order, replayed, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
		CustomerID:     "customer-1",
		IdempotencyKey: "key-1",
		Items: []store.OrderItemInput{
			{ProductID: coffee, Quantity: 2},
			{ProductID: mug, Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if replayed {
		t.Fatal("first request must not be a replay")
	}

	if want := int64(129900*2 + 39900); order.TotalCents != want {
		t.Errorf("total = %d, want %d", order.TotalCents, want)
	}
	if order.Status != store.StatusPending {
		t.Errorf("status = %q, want %q", order.Status, store.StatusPending)
	}
	if got := db.Stock(t, coffee); got != 8 {
		t.Errorf("coffee stock = %d, want 8", got)
	}
	if got := db.Stock(t, mug); got != 4 {
		t.Errorf("mug stock = %d, want 4", got)
	}
	if len(order.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(order.Items))
	}
}

func TestConcurrentCheckoutOfLastItem(t *testing.T) {
	db := testdb.New(t)
	last := db.SeedProduct(t, "SKU-LAST", 100000, 1)

	const buyers = 20
	var (
		wg       sync.WaitGroup
		success  atomic.Int32
		soldOut  atomic.Int32
		unexpect atomic.Int32
	)

	start := make(chan struct{})
	for i := range buyers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			_, _, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
				CustomerID:     fmt.Sprintf("customer-%d", i),
				IdempotencyKey: fmt.Sprintf("key-%d", i),
				Items:          []store.OrderItemInput{{ProductID: last, Quantity: 1}},
			})

			var stockErr *store.InsufficientStockError
			switch {
			case err == nil:
				success.Add(1)
			case errors.As(err, &stockErr):
				soldOut.Add(1)
			default:
				unexpect.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Errorf("successful orders = %d, want exactly 1", got)
	}
	if got := soldOut.Load(); got != buyers-1 {
		t.Errorf("sold-out errors = %d, want %d", got, buyers-1)
	}
	if got := unexpect.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}
	if got := db.Stock(t, last); got != 0 {
		t.Errorf("stock = %d, want 0 (no oversell)", got)
	}
}

func TestRepeatedRequestCreatesOneOrder(t *testing.T) {
	db := testdb.New(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 129900, 10)

	input := store.CreateOrderInput{
		CustomerID:     "customer-1",
		IdempotencyKey: "same-key",
		Items:          []store.OrderItemInput{{ProductID: product, Quantity: 3}},
	}

	first, replayed, err := db.Store.CreateOrder(context.Background(), input)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if replayed {
		t.Fatal("first request must not be a replay")
	}

	second, replayed, err := db.Store.CreateOrder(context.Background(), input)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if !replayed {
		t.Fatal("second request must be a replay")
	}
	if second.ID != first.ID {
		t.Errorf("replay returned order %s, want %s", second.ID, first.ID)
	}
	if got := db.Stock(t, product); got != 7 {
		t.Errorf("stock = %d, want 7 (reserved once)", got)
	}
}

func TestSameKeyWithDifferentPayloadIsRejected(t *testing.T) {
	db := testdb.New(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 129900, 10)

	if _, _, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
		CustomerID:     "customer-1",
		IdempotencyKey: "reused-key",
		Items:          []store.OrderItemInput{{ProductID: product, Quantity: 1}},
	}); err != nil {
		t.Fatalf("first request: %v", err)
	}

	_, _, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
		CustomerID:     "customer-1",
		IdempotencyKey: "reused-key",
		Items:          []store.OrderItemInput{{ProductID: product, Quantity: 5}},
	})
	if !errors.Is(err, store.ErrKeyReuse) {
		t.Fatalf("err = %v, want ErrKeyReuse", err)
	}
}

func TestUnknownProductIsReported(t *testing.T) {
	db := testdb.New(t)

	_, _, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
		CustomerID:     "customer-1",
		IdempotencyKey: "key-1",
		Items:          []store.OrderItemInput{{ProductID: 999999, Quantity: 1}},
	})
	if !errors.Is(err, store.ErrProductNotFound) {
		t.Fatalf("err = %v, want ErrProductNotFound", err)
	}
}

func TestListFiltersAndPaginates(t *testing.T) {
	db := testdb.New(t)
	product := db.SeedProduct(t, "SKU-COFFEE", 1000, 100)

	for i := range 5 {
		if _, _, err := db.Store.CreateOrder(context.Background(), store.CreateOrderInput{
			CustomerID:     "customer-1",
			IdempotencyKey: fmt.Sprintf("key-%d", i),
			Items:          []store.OrderItemInput{{ProductID: product, Quantity: 1}},
		}); err != nil {
			t.Fatalf("create order %d: %v", i, err)
		}
	}

	orders, total, err := db.Store.List(context.Background(), store.StatusPending, 2, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(orders) != 2 {
		t.Errorf("page size = %d, want 2", len(orders))
	}
	if len(orders[0].Items) != 1 {
		t.Errorf("items loaded = %d, want 1", len(orders[0].Items))
	}

	paid, total, err := db.Store.List(context.Background(), store.StatusPaid, 10, 0)
	if err != nil {
		t.Fatalf("list paid: %v", err)
	}
	if total != 0 || len(paid) != 0 {
		t.Errorf("paid orders = %d (total %d), want none", len(paid), total)
	}
}
