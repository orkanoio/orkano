package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/orkanoio/orkano/receiver/internal/webhook"
)

type fakePinger struct {
	calls int
	err   error
}

func (f *fakePinger) Ping(context.Context) error {
	f.calls++
	return f.err
}

func TestReadinessRequiresValidAllowlistAndDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repositories")
	if err := os.WriteFile(path, []byte("orkanoio/orkano\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	h := webhook.NewHandler(webhook.Config{
		AllowlistSource: webhook.FileAllowlist{Path: path},
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := &fakePinger{}
	ready := readinessHandler(log, db, h)

	assertReadinessStatus(t, ready, http.StatusOK)
	if db.calls != 1 {
		t.Fatalf("database ping calls = %d, want 1", db.calls)
	}

	if err := os.WriteFile(path, []byte("owner/*\n"), 0o600); err != nil {
		t.Fatalf("write malformed policy: %v", err)
	}
	assertReadinessStatus(t, ready, http.StatusServiceUnavailable)
	if db.calls != 1 {
		t.Fatalf("database was pinged with invalid policy; calls = %d, want 1", db.calls)
	}

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty policy: %v", err)
	}
	assertReadinessStatus(t, ready, http.StatusOK)
	if db.calls != 2 {
		t.Fatalf("valid empty deny-all policy should be ready; ping calls = %d, want 2", db.calls)
	}

	db.err = errors.New("database down")
	assertReadinessStatus(t, ready, http.StatusServiceUnavailable)
}

func TestReadinessFailsWhenAllowlistFileIsMissing(t *testing.T) {
	h := webhook.NewHandler(webhook.Config{
		AllowlistSource: webhook.FileAllowlist{Path: filepath.Join(t.TempDir(), "missing")},
	})
	db := &fakePinger{}
	ready := readinessHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), db, h)
	assertReadinessStatus(t, ready, http.StatusServiceUnavailable)
	if db.calls != 0 {
		t.Fatalf("database ping calls = %d, want 0 when policy is unavailable", db.calls)
	}
}

func assertReadinessStatus(t *testing.T, handler http.HandlerFunc, want int) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != want {
		t.Fatalf("readiness status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}
