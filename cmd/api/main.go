// file: cmd/api/main.go  (replace)
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/AnishPrakash/arena/internal/adapters/postgres"
)

var version = "dev"

func main() {
	ctx := context.Background()
	dsn := os.Getenv("ARENA_PG_DSN")
	if dsn == "" {
		dsn = "postgres://arena:arena@localhost:5432/arena?sslmode=disable"
	}

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		st, err := postgres.New(ctx, dsn, 5)
		if err != nil {
			log.Fatal(err)
		}
		defer st.Close()
		if err := st.Migrate(ctx); err != nil {
			log.Fatal(err)
		}
		fmt.Println("migrations up to date")
		return
	}

	fmt.Println("arena api", version, "- HTTP server arrives in Phase 3")
}
