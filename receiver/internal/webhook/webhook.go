// Package webhook is the internet-facing GitHub webhook receiver's request
// handling. It is a doorbell, not data (INV-04): it verifies the HMAC
// signature, drops anything not from an allowlisted repository, and records a
// pointer row in the delivery queue. It never trusts the payload beyond the
// repository name it stores, never reaches the cluster or GitHub, and holds no
// credential but the HMAC key and an INSERT-only Postgres role — the operator's
// dispatcher re-fetches every authoritative detail from the GitHub API.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/orkanoio/orkano/internal/db"
)

// DefaultMaxBodyBytes bounds how much of a request body the receiver will read
// before computing the HMAC. GitHub caps webhook payloads at 25 MB; an
// internet-facing endpoint must never read an unbounded body into memory.
const DefaultMaxBodyBytes int64 = 25 << 20

// GitHub event names (the X-GitHub-Event header). Only push events drive a
// deploy; ping is GitHub's webhook-creation handshake. Everything else is
// acknowledged and dropped pre-queue so the queue carries only actionable rows.
const (
	eventPush = "push"
	eventPing = "ping"
)

// Field length bounds mirror the CHECK constraints in internal/db migration
// 00001_webhook_deliveries.sql. Validating here turns malformed input into a
// clean 400 instead of a database error, and keeps the handler decoupled from
// the DB driver. The migration's CHECKs remain the authoritative guard.
const (
	maxDeliveryIDLen = 72
	maxRepoLen       = 200
	maxEventTypeLen  = 50
)

const signatureHeader = "X-Hub-Signature-256"

// Enqueuer records a delivery pointer. *db.Queries satisfies it; tests
// substitute a fake so the request path can be exercised without a database.
type Enqueuer interface {
	EnqueueDelivery(ctx context.Context, arg db.EnqueueDeliveryParams) (int64, error)
}

// Config is the complete configuration of the handler: the HMAC key, the repo
// allowlist, and where to enqueue. There is deliberately nothing else.
type Config struct {
	// Secret is the GitHub webhook HMAC key. Required and non-empty.
	Secret []byte
	// Allowlist holds raw "owner/repo" entries; they are trimmed, lowercased,
	// and de-duplicated here so lookup is a single normalized comparison. An
	// empty allowlist rejects every repository (fail closed).
	Allowlist []string
	// Enqueuer is the delivery-queue sink (an INSERT-only Postgres role in
	// production).
	Enqueuer Enqueuer
	// Logger receives structured request logs; nil falls back to slog's default.
	Logger *slog.Logger
	// MaxBody bounds the request body; 0 uses DefaultMaxBodyBytes.
	MaxBody int64
}

// Handler serves the webhook endpoint.
type Handler struct {
	secret    []byte
	allowlist map[string]struct{}
	enqueuer  Enqueuer
	log       *slog.Logger
	maxBody   int64
}

// NewHandler builds a Handler from cfg, normalizing the allowlist once.
func NewHandler(cfg Config) *Handler {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	maxBody := cfg.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	allow := make(map[string]struct{}, len(cfg.Allowlist))
	for _, e := range cfg.Allowlist {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			allow[e] = struct{}{}
		}
	}
	return &Handler{
		secret:    cfg.Secret,
		allowlist: allow,
		enqueuer:  cfg.Enqueuer,
		log:       log,
		maxBody:   maxBody,
	}
}

// AllowlistSize returns the number of normalized repositories in the allowlist.
// An empty allowlist rejects everything, so the command warns on it at startup.
func (h *Handler) AllowlistSize() int { return len(h.allowlist) }

// Webhook handles one GitHub webhook delivery. The order is load-bearing: the
// signature is verified over the raw bytes before the payload is parsed or
// acted on (INV-04 — distrust until proven authentic).
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.reject(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.reject(w, r, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		h.reject(w, r, http.StatusBadRequest, "cannot read body")
		return
	}

	if !h.validSignature(r.Header.Get(signatureHeader), body) {
		h.reject(w, r, http.StatusUnauthorized, "signature verification failed")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case eventPing:
		// GitHub's creation handshake. App-level pings carry no repository, so
		// acknowledge without parsing or enqueueing.
		h.ack(w, r, "pong", event)
		return
	case eventPush:
		// fall through to enqueue
	default:
		h.ack(w, r, "ignored", event)
		return
	}

	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		h.reject(w, r, http.StatusBadRequest, "missing delivery id")
		return
	}

	// Parse the minimal surface: the repository name, only to enforce the
	// allowlist and store the pointer.
	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		h.reject(w, r, http.StatusBadRequest, "invalid json")
		return
	}
	repo := payload.Repository.FullName
	if repo == "" {
		h.reject(w, r, http.StatusBadRequest, "missing repository")
		return
	}

	if !h.allowed(repo) {
		// Drop pre-enqueue: the allowlist is the noise filter (threat model).
		h.reject(w, r, http.StatusForbidden, "repository not allowed")
		return
	}

	if len(delivery) > maxDeliveryIDLen || len(repo) > maxRepoLen || len(event) > maxEventTypeLen {
		h.reject(w, r, http.StatusBadRequest, "field too long")
		return
	}

	rows, err := h.enqueuer.EnqueueDelivery(r.Context(), db.EnqueueDeliveryParams{
		DeliveryID: delivery,
		Repo:       repo,
		EventType:  event,
	})
	if err != nil {
		h.log.Error("enqueue failed", "remote", r.RemoteAddr, "repo", repo, "err", err)
		h.reject(w, r, http.StatusInternalServerError, "enqueue failed")
		return
	}

	// rows == 0 means a duplicate X-GitHub-Delivery collapsed via ON CONFLICT
	// DO NOTHING — still success, the doorbell already rang.
	result := "enqueued"
	if rows == 0 {
		result = "duplicate"
	}
	h.log.Info("delivery accepted", "repo", repo, "delivery", delivery, "result", result)
	h.respond(w, http.StatusAccepted, "accepted")
}

// validSignature reports whether header is a valid sha256 HMAC of body under the
// handler's secret. The comparison is constant time (hmac.Equal); the sha256 hex
// digest is fixed length, so comparing the full "sha256=<hex>" strings leaks
// nothing about the secret.
func (h *Handler) validSignature(header string, body []byte) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha256.New, h.secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

func (h *Handler) allowed(repo string) bool {
	_, ok := h.allowlist[strings.ToLower(repo)]
	return ok
}

func (h *Handler) ack(w http.ResponseWriter, r *http.Request, msg, event string) {
	h.log.Info("delivery acknowledged", "event", event, "result", msg)
	h.respond(w, http.StatusOK, msg)
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.log.Warn("delivery rejected", "remote", r.RemoteAddr, "status", status, "reason", msg)
	h.respond(w, status, msg)
}

func (h *Handler) respond(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
