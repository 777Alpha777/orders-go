// Command migrate applies the embedded migrations and exits. The API applies
// them on start too; this binary exists for pipelines that want the schema step
// to be separate from the deployment.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/777Alpha777/orders-go/internal/config"
	"github.com/777Alpha777/orders-go/internal/store"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	fmt.Println("migrations applied")
}
