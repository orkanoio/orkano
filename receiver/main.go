// Command orkano-receiver is the internet-facing GitHub webhook receiver. Its
// entire configuration is the HMAC key, a Postgres DSN (an INSERT-only role),
// and a repo allowlist; it holds no cluster or GitHub credentials (INV-04).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/receiver/internal/webhook"
)

var version = "dev"

const (
	//nolint:gosec // G101: this is the env var *name* the secret is read from, not a credential.
	envSecret    = "ORKANO_WEBHOOK_SECRET"
	envDSN       = "ORKANO_DB_DSN"
	envAllowlist = "ORKANO_REPO_ALLOWLIST"
	envAddr      = "ORKANO_ADDR"
	defaultAddr  = ":8080"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("orkano-receiver exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	secret := os.Getenv(envSecret)
	if secret == "" {
		return fmt.Errorf("%s is required", envSecret)
	}
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		return fmt.Errorf("%s is required", envDSN)
	}
	allowlist := strings.Split(os.Getenv(envAllowlist), ",")
	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("create db pool: %w", err)
	}
	defer pool.Close()

	h := webhook.NewHandler(webhook.Config{
		Secret:    []byte(secret),
		Allowlist: allowlist,
		Enqueuer:  db.New(pool),
		Logger:    log,
	})
	if h.AllowlistSize() == 0 {
		log.Warn("repo allowlist is empty; every webhook will be rejected", "env", envAllowlist)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", h.Webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		// An empty-statement ping needs no table privilege, so it works under
		// the INSERT-only receiver role while still proving the DB is reachable.
		if err := pool.Ping(pingCtx); err != nil {
			log.Warn("readiness ping failed", "err", err)
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("orkano-receiver listening", "addr", addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
