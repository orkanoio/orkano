// Command orkano-dashboard is the operator-facing control panel. It serves the
// embedded SPA plus a small Go API and writes Orkano custom resources through a
// narrow-RBAC ServiceAccount (INV-01: never cluster-admin). It is ClusterIP-only
// — never internet-reachable by default (INV-05); exposure is the onboarding
// wizard's job (orkano proxy / Tailscale / IAP).
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

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/dashboard/internal/server"
	"github.com/orkanoio/orkano/dashboard/web"
)

var version = "dev"

const (
	envDSN       = "ORKANO_DB_DSN"
	envAddr      = "ORKANO_ADDR"
	envEncKey    = "ORKANO_DASHBOARD_ENC_KEY"
	envTokenHash = "ORKANO_BOOTSTRAP_TOKEN_SHA256" //nolint:gosec // G101: env var name, not a credential
	defaultAddr  = ":8080"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("orkano-dashboard exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		return fmt.Errorf("%s is required", envDSN)
	}
	encKey := os.Getenv(envEncKey)
	if encKey == "" {
		return fmt.Errorf("%s is required", envEncKey)
	}
	tokenHash := os.Getenv(envTokenHash)
	if tokenHash == "" {
		return fmt.Errorf("%s is required", envTokenHash)
	}
	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}

	cipher, err := auth.NewCipher(encKey)
	if err != nil {
		return fmt.Errorf("build cipher: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("create db pool: %w", err)
	}
	defer pool.Close()

	k8s, restCfg, err := newK8sClient()
	if err != nil {
		return err
	}
	// Read views run through a client impersonating the fixed, resourceNames-
	// pinned viewer identity, so the cluster's RBAC + audit trail see a view-only
	// identity, not the dashboard SA (ADR-0013/ADR-0015). It reuses the base
	// client's scheme + RESTMapper to skip discovery.
	viewerClient, err := server.NewViewerClient(restCfg, k8s.Scheme(), k8s.RESTMapper())
	if err != nil {
		return fmt.Errorf("create viewer client: %w", err)
	}

	srv, err := server.New(server.Config{
		K8s:                k8s,
		ViewerClient:       viewerClient,
		DB:                 pool,
		Store:              server.NewStore(pool),
		Cipher:             cipher,
		BootstrapTokenHash: tokenHash,
		SPA:                web.Assets(),
		Logger:             log,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("orkano-dashboard listening", "addr", addr, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// newK8sClient builds the controller-runtime client the dashboard uses to write
// Orkano custom resources. The scheme carries the orkano.io types plus core (for
// the value-blind Secret writes of ADR-0013); RBAC — not the scheme — bounds
// what the client may actually do (the orkano-dashboard Role).
func newK8sClient() (client.Client, *rest.Config, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(orkanov1alpha1.AddToScheme(scheme))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load kube config: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("create kube client: %w", err)
	}
	return c, cfg, nil
}
