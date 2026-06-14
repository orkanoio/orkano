package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/receiver/internal/webhook"
)

const (
	headerEvent     = "X-GitHub-Event"
	headerDelivery  = "X-GitHub-Delivery"
	headerSignature = "X-Hub-Signature-256"
)

var testSecret = []byte("orkano-test-secret")

// sign returns the X-Hub-Signature-256 value GitHub would send for body under
// key — the same construction the handler verifies against.
func sign(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type fakeEnqueuer struct {
	calls []db.EnqueueDeliveryParams
	rows  int64
	err   error
}

func (f *fakeEnqueuer) EnqueueDelivery(_ context.Context, arg db.EnqueueDeliveryParams) (int64, error) {
	f.calls = append(f.calls, arg)
	return f.rows, f.err
}

const (
	validRepo = "orkanoio/orkano"
	validBody = `{"repository":{"full_name":"orkanoio/orkano"}}`
)

func TestWebhook(t *testing.T) {
	cases := []struct {
		name         string
		method       string
		event        string
		delivery     string
		body         string
		mkSig        func(body []byte) string // nil → correct signature over body
		allowlist    []string
		enqRows      int64
		enqErr       error
		wantStatus   int
		wantEnqueued bool
	}{
		{
			name:         "valid push enqueued",
			method:       http.MethodPost,
			event:        "push",
			delivery:     "del-1",
			body:         validBody,
			allowlist:    []string{validRepo},
			enqRows:      1,
			wantStatus:   http.StatusAccepted,
			wantEnqueued: true,
		},
		{
			name:         "duplicate delivery still accepted",
			method:       http.MethodPost,
			event:        "push",
			delivery:     "del-1",
			body:         validBody,
			allowlist:    []string{validRepo},
			enqRows:      0, // ON CONFLICT DO NOTHING
			wantStatus:   http.StatusAccepted,
			wantEnqueued: true,
		},
		{
			name:       "missing signature",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			mkSig:      func([]byte) string { return "" },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "tampered body",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			mkSig:      func(b []byte) string { return sign(testSecret, append(b, 'x')) },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong key",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			mkSig:      func(b []byte) string { return sign([]byte("not-the-secret"), b) },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed signature hex",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			mkSig:      func([]byte) string { return "sha256=not-valid-hex" },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "signature without scheme prefix",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			mkSig:      func(b []byte) string { return strings.TrimPrefix(sign(testSecret, b), "sha256=") },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bad signature checked before json parse",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       "this is not json",
			mkSig:      func([]byte) string { return "" },
			allowlist:  []string{validRepo},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "repository not allowlisted",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			allowlist:  []string{"someone/else"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "empty allowlist rejects all",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			allowlist:  nil,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong method",
			method:     http.MethodGet,
			event:      "push",
			delivery:   "del-1",
			body:       validBody,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "ping acknowledged not enqueued",
			method:     http.MethodPost,
			event:      "ping",
			body:       `{"zen":"keep it logically awesome"}`,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusOK,
		},
		{
			name:       "unrelated event ignored",
			method:     http.MethodPost,
			event:      "issues",
			delivery:   "del-1",
			body:       validBody,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing delivery id",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "",
			body:       validBody,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid json",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       "{not json",
			allowlist:  []string{validRepo},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing repository",
			method:     http.MethodPost,
			event:      "push",
			delivery:   "del-1",
			body:       `{"ref":"refs/heads/main"}`,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversize delivery id",
			method:     http.MethodPost,
			event:      "push",
			delivery:   strings.Repeat("a", 73),
			body:       validBody,
			allowlist:  []string{validRepo},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:         "enqueue error surfaces as 500",
			method:       http.MethodPost,
			event:        "push",
			delivery:     "del-1",
			body:         validBody,
			allowlist:    []string{validRepo},
			enqErr:       errors.New("db down"),
			wantStatus:   http.StatusInternalServerError,
			wantEnqueued: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEnqueuer{rows: tc.enqRows, err: tc.enqErr}
			h := webhook.NewHandler(webhook.Config{
				Secret:    testSecret,
				Allowlist: tc.allowlist,
				Enqueuer:  fake,
			})

			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/webhook", strings.NewReader(tc.body))
			if tc.event != "" {
				req.Header.Set(headerEvent, tc.event)
			}
			if tc.delivery != "" {
				req.Header.Set(headerDelivery, tc.delivery)
			}
			sig := sign(testSecret, []byte(tc.body))
			if tc.mkSig != nil {
				sig = tc.mkSig([]byte(tc.body))
			}
			if sig != "" {
				req.Header.Set(headerSignature, sig)
			}

			rec := httptest.NewRecorder()
			h.Webhook(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantEnqueued && len(fake.calls) != 1 {
				t.Errorf("enqueue calls = %d, want 1", len(fake.calls))
			}
			if !tc.wantEnqueued && len(fake.calls) != 0 {
				t.Errorf("enqueue calls = %d, want 0 (must reject before enqueue)", len(fake.calls))
			}
			if tc.wantEnqueued && tc.enqErr == nil && len(fake.calls) == 1 {
				got := fake.calls[0]
				if got.DeliveryID != tc.delivery || got.Repo != validRepo || got.EventType != tc.event {
					t.Errorf("enqueued %+v, want delivery=%q repo=%q event=%q",
						got, tc.delivery, validRepo, tc.event)
				}
			}
		})
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	fake := &fakeEnqueuer{rows: 1}
	h := webhook.NewHandler(webhook.Config{
		Secret:    testSecret,
		Allowlist: []string{validRepo},
		Enqueuer:  fake,
		MaxBody:   8,
	})

	body := []byte(validBody) // larger than 8 bytes
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "push")
	req.Header.Set(headerDelivery, "del-1")
	req.Header.Set(headerSignature, sign(testSecret, body))

	rec := httptest.NewRecorder()
	h.Webhook(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if len(fake.calls) != 0 {
		t.Errorf("enqueue calls = %d, want 0", len(fake.calls))
	}
}

func TestAllowlistNormalization(t *testing.T) {
	h := webhook.NewHandler(webhook.Config{
		Secret:    testSecret,
		Allowlist: []string{"  Orkanoio/Orkano  ", "", "dup/repo", "dup/repo"},
		Enqueuer:  &fakeEnqueuer{},
	})
	if got := h.AllowlistSize(); got != 2 {
		t.Fatalf("AllowlistSize = %d, want 2 (trim/dedupe)", got)
	}

	// A differently-cased repository from the payload still matches.
	fake := &fakeEnqueuer{rows: 1}
	h = webhook.NewHandler(webhook.Config{
		Secret:    testSecret,
		Allowlist: []string{"orkanoio/orkano"},
		Enqueuer:  fake,
	})
	body := `{"repository":{"full_name":"OrkanoIO/Orkano"}}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(headerEvent, "push")
	req.Header.Set(headerDelivery, "del-1")
	req.Header.Set(headerSignature, sign(testSecret, []byte(body)))

	rec := httptest.NewRecorder()
	h.Webhook(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(fake.calls) != 1 || fake.calls[0].Repo != "OrkanoIO/Orkano" {
		t.Fatalf("expected enqueue preserving payload repo case, got %+v", fake.calls)
	}
}
