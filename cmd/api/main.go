// file: cmd/api/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AnishPrakash/arena/internal/adapters/blob"
	"github.com/AnishPrakash/arena/internal/adapters/postgres"
	"github.com/AnishPrakash/arena/internal/adapters/redisq"
	"github.com/AnishPrakash/arena/internal/api"
	"github.com/AnishPrakash/arena/internal/config"
	"github.com/AnishPrakash/arena/internal/langs"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo}[cfg.LogLevel],
	}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := postgres.New(ctx, cfg.PGDSN, cfg.PGMaxConn)
	fatal(log, err)
	defer store.Close()

	// Migrating on boot (guarded by an advisory lock) is what makes "clone and run" true.
	fatal(log, store.Migrate(ctx))

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		fmt.Println("migrations up to date")
		return
	}

	rdb, err := redisq.NewClient(ctx, cfg.RedisAddr, cfg.RedisDB)
	fatal(log, err)
	defer rdb.Close()

	queue, err := redisq.NewQueue(ctx, rdb, redisq.QueueOpts{
		Stream: cfg.Stream, Group: cfg.Group, LeaseTTL: cfg.LeaseTTL,
	})
	fatal(log, err)

	blobs, err := blob.NewLocal(cfg.BlobRoot)
	fatal(log, err)

	registry, err := langs.LoadBuiltin()
	fatal(log, err)
	log.Info("languages loaded", "ids", registry.IDs())

	srv := api.NewServer(api.Deps{
		Cfg: cfg, Log: log, Store: store, Queue: queue, Blobs: blobs,
		Board: redisq.NewLeaderboard(rdb), Bus: redisq.NewBus(rdb),
		RL: redisq.NewRateLimiter(rdb), Langs: registry,
	})

	srv.StartReconciler(ctx, 30*time.Second, 2*time.Minute)
	srv.StartQueueGauges(ctx)

	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: srv.Router(),
		// ReadHeaderTimeout defends against Slowloris: a client that opens a connection
		// and dribbles headers forever otherwise pins a goroutine indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout must be 0 (unlimited) because SSE streams are long-lived; the SSE
		// handler enforces its own 5-minute deadline instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "version", version, "env", cfg.Env)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	// Drain in-flight requests before exiting so a rolling deploy never drops a submission
	// that has already been accepted.
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	log.Info("bye")
}

func fatal(log *slog.Logger, err error) {
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
