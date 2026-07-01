package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakePinger stands in for the DB pool in /readyz tests.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// blockingPinger blocks until its context is cancelled, so a test can prove
// /readyz bounds the ping by the request/handler context.
type blockingPinger struct{}

func (blockingPinger) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

const indexBody = "<!doctype html><title>orkano</title><div id=root>spa</div>"

// testSPA is a minimal embedded-app stand-in: an index shell, one hashed
// asset, and one root-level file (Vite's public/ passthrough).
func testSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte(indexBody)},
		"assets/app.js": {Data: []byte("console.log('orkano')")},
		"orkano.svg":    {Data: []byte("<svg/>")},
	}
}

func newTestServer(t *testing.T, db Pinger) *Server {
	t.Helper()
	k8s := fake.NewClientBuilder().Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            &fakePodStreamer{},
		DB:                 db,
		Store:              newFakeStore(),
		Cipher:             testCipher(t),
		BootstrapTokenHash: "deadbeef",
		SPA:                testSPA(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func do(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNewValidatesConfig(t *testing.T) {
	okClient := func() client.Client { return fake.NewClientBuilder().Build() }
	// full is a complete config; each case zeroes exactly one required field, so a
	// rejection proves that field is required (not some other omission).
	full := func() Config {
		return Config{
			K8s:                okClient(),
			ViewerClient:       okClient(),
			PodLogs:            &fakePodStreamer{},
			DB:                 fakePinger{},
			Store:              newFakeStore(),
			Cipher:             testCipher(t),
			BootstrapTokenHash: "deadbeef",
			SPA:                testSPA(),
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"nil k8s", func(c *Config) { c.K8s = nil }},
		{"nil viewer client", func(c *Config) { c.ViewerClient = nil }},
		{"nil pod logs", func(c *Config) { c.PodLogs = nil }},
		{"nil db", func(c *Config) { c.DB = nil }},
		{"nil spa", func(c *Config) { c.SPA = nil }},
		{"nil store", func(c *Config) { c.Store = nil }},
		{"nil cipher", func(c *Config) { c.Cipher = nil }},
		{"empty token hash", func(c *Config) { c.BootstrapTokenHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("expected New to reject the incomplete config")
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := do(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Errorf("healthz body = %q, want %q", rec.Body.String(), "ok\n")
	}
}

func TestReadyz(t *testing.T) {
	t.Run("db healthy", func(t *testing.T) {
		s := newTestServer(t, fakePinger{})
		if rec := do(t, s, http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d, want 200", rec.Code)
		}
	})
	t.Run("db down", func(t *testing.T) {
		s := newTestServer(t, fakePinger{err: errors.New("connection refused")})
		if rec := do(t, s, http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d, want 503", rec.Code)
		}
	})
	t.Run("ping is context-bounded", func(t *testing.T) {
		// A short request deadline wins over readyTimeout, so the blocking ping is
		// cancelled and 503s promptly instead of hanging the probe.
		s := newTestServer(t, blockingPinger{})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil)
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d, want 503", rec.Code)
		}
	})
}

func TestSPAServesRealFile(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := do(t, s, http.MethodGet, "/assets/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log('orkano')" {
		t.Errorf("asset body = %q", rec.Body.String())
	}
	// Everything under assets/ is content-hashed by Vite, so it must be cached
	// forever (embedded files carry no modtime — without the header every asset
	// would re-download on each page load).
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}

	// Root-level files (Vite public/ passthrough) are NOT content-hashed, so
	// they must not get the immutable header.
	rec = do(t, s, http.MethodGet, "/orkano.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("public file status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("public file Cache-Control = %q, want unset", cc)
	}
}

func TestSPAFallsBackToIndex(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	// Root, a dot-path, and an unknown client-side route all serve index.html so
	// deep links into the SPA resolve rather than 404.
	for _, target := range []string{"/", "/./", "/apps/my-app/settings"} {
		rec := do(t, s, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", target, rec.Code)
		}
		if rec.Body.String() != indexBody {
			t.Errorf("%s did not serve index.html (got %q)", target, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s content-type = %q", target, ct)
		}
	}
}

// A traversal attempt must not escape the embedded tree; path.Clean collapses it
// and fs.Stat misses, so it falls back to the SPA index rather than reading the
// host filesystem.
func TestSPARejectsTraversal(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := do(t, s, http.MethodGet, "/../../etc/passwd")
	if rec.Code != http.StatusOK || rec.Body.String() != indexBody {
		t.Fatalf("traversal leaked (expected SPA index): status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestServeIndexMissing(t *testing.T) {
	// An SPA tree with no index.html surfaces a 500, not a panic — proves the
	// fallback's error path.
	k8s := fake.NewClientBuilder().Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            &fakePodStreamer{},
		DB:                 fakePinger{},
		Store:              newFakeStore(),
		Cipher:             testCipher(t),
		BootstrapTokenHash: "deadbeef",
		SPA:                fstest.MapFS{"assets/app.js": {Data: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rec := do(t, s, http.MethodGet, "/"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing index status = %d, want 500", rec.Code)
	}
}
