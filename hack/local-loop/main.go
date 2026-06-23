// Command local-loop-helper is the dev-only sidekick for `make local-loop`
// (hack/local-loop/run.sh). It bundles the few bits of glue the event-path loop
// needs that are awkward in shell: applying the platform migrations + role
// passwords, standing in for the GitHub API the dispatcher re-fetches commits
// from, minting a throwaway GitHub App private key, and signing a webhook body
// exactly the way the receiver verifies it. It talks to nothing outside the
// loop's own localhost Postgres + kind cluster.
//
// Unlike the hack/spikes (each its own throwaway module), this helper lives IN
// the root module on purpose: it reuses internal/db.Migrate — the canonical,
// only-importable-from-within-the-root-module migration path — so `make
// build/lint/test/vulncheck` and CI cover it like any other root code and it
// can't silently rot. It is only ever excluded from release artifacts
// (goreleaser builds ./cli, ./operator, ./receiver — never this).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/orkanoio/orkano/internal/db"
)

// stubToken is the fake installation token the GitHub stub returns. It is never
// a real credential — the operator carries it as a bearer token to the stub,
// which ignores it.
//
//nolint:gosec // G101: a fake stub token, not a credential.
const stubToken = "ghs_localloopstubtoken"

// rolePassword is the password the loop sets on both least-privilege roles.
// The loop's Postgres is a throwaway container bound to 127.0.0.1, so a fixed
// dev password is fine and keeping it a constant means the ALTER ROLE SQL is
// constant too (no string-built SQL). run.sh never needs to know it: `migrate`
// prints the fully-formed role DSNs on stdout.
const rolePassword = "localloop"

func main() {
	log.SetFlags(0)
	log.SetPrefix("local-loop-helper: ")
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "migrate":
		runMigrate(os.Args[2:])
	case "github-stub":
		runStub(os.Args[2:])
	case "genkey":
		runGenKey(os.Args[2:])
	case "sign":
		runSign(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	log.Fatal("usage: local-loop-helper {migrate|github-stub|genkey|sign} [flags]")
}

// runMigrate applies the platform schema + roles to the superuser DSN, sets the
// role passwords the migration deliberately leaves unset (init's job in prod),
// and prints the receiver/dispatcher DSNs derived from the superuser DSN so the
// script never has to know the password.
func runMigrate(argv []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("dsn", "", "superuser Postgres DSN to migrate (required)")
	_ = fs.Parse(argv)
	if *dsn == "" {
		log.Fatal("migrate: --dsn is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.Migrate(ctx, *dsn); err != nil {
		log.Fatalf("migrate: applying migrations: %v", err)
	}
	// Both least-privilege roles get the same fixed dev password — the loop's
	// Postgres is a throwaway container bound to 127.0.0.1. SetupRoles is the
	// same install-time path the platform's migration Job runs in prod.
	if err := db.SetupRoles(ctx, *dsn, rolePassword, rolePassword); err != nil {
		log.Fatalf("migrate: setting role passwords: %v", err)
	}

	recv, err := roleDSN(*dsn, "orkano_receiver")
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	disp, err := roleDSN(*dsn, "orkano_dispatcher")
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	// stdout carries only these two KEY=VALUE lines; logs go to stderr.
	fmt.Printf("RECEIVER_DSN=%s\n", recv)
	fmt.Printf("DISPATCHER_DSN=%s\n", disp)
}

// roleDSN rewrites the superuser DSN's userinfo to a least-privilege role and
// the loop's dev password, preserving host, database, and query (e.g.
// sslmode=disable).
func roleDSN(superuser, role string) (string, error) {
	u, err := url.Parse(superuser)
	if err != nil {
		return "", fmt.Errorf("parsing dsn: %w", err)
	}
	u.User = url.UserPassword(role, rolePassword)
	return u.String(), nil
}

// runStub serves the slice of the GitHub REST API the dispatcher's commit
// resolution touches: the installation lookup, the installation-token mint, the
// repo (for the default branch), and the commit. Every repo resolves to the
// same canned SHA — the loop proves the event path, not GitHub.
func runStub(argv []string) {
	fs := flag.NewFlagSet("github-stub", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9099", "address to listen on")
	sha := fs.String("sha", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "40-char commit SHA every ref resolves to")
	branch := fs.String("default-branch", "main", "default branch reported for every repo")
	installID := fs.Int64("installation-id", 42, "installation id every repo lookup returns")
	_ = fs.Parse(argv)

	mux := http.NewServeMux()
	// POST /app/installations/{id}/access_tokens — mint an installation token.
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		logHit("access_tokens")
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      stubToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	// GET /repos/{owner}/{name}/installation — which installation covers the repo.
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, _ *http.Request) {
		logHit("installation")
		writeJSON(w, http.StatusOK, map[string]any{"id": *installID})
	})
	// GET /repos/{owner}/{name}/commits/{ref...} — the HEAD SHA at a ref (ref may
	// contain slashes, e.g. release/1.x).
	mux.HandleFunc("GET /repos/{owner}/{name}/commits/{ref...}", func(w http.ResponseWriter, _ *http.Request) {
		logHit("commits")
		writeJSON(w, http.StatusOK, map[string]any{"sha": *sha})
	})
	// GET /repos/{owner}/{name} — repo metadata, read only for default_branch.
	mux.HandleFunc("GET /repos/{owner}/{name}", func(w http.ResponseWriter, _ *http.Request) {
		logHit("repo")
		writeJSON(w, http.StatusOK, map[string]any{"default_branch": *branch})
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("github-stub listening on %s (sha=%s default-branch=%s installation-id=%d)",
		*addr, *sha, *branch, *installID)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("github-stub: %v", err)
	}
}

// logHit logs which stub endpoint was hit. The label is a static literal, never
// request-derived, so nothing untrusted reaches the log.
func logHit(label string) { log.Printf("github-stub %s", label) }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// runGenKey emits a fresh PKCS#1 RSA private key to stdout — the GitHub App
// signing key the operator reads from the orkano-github-app Secret. The stub
// never verifies the JWT, but the operator must sign one with a real key.
func runGenKey(argv []string) {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	_ = fs.Parse(argv)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("genkey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if err := pem.Encode(os.Stdout, block); err != nil {
		log.Fatalf("genkey: %v", err)
	}
}

// runSign reads a webhook body from stdin and prints the X-Hub-Signature-256
// header value the receiver checks: "sha256=" + hex(HMAC-SHA256(body, secret)).
// Computing it here (not via openssl) guarantees the bytes match the receiver's
// crypto/hmac path exactly.
func runSign(argv []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	secret := fs.String("secret", "", "HMAC secret (required)")
	_ = fs.Parse(argv)
	if *secret == "" {
		log.Fatal("sign: --secret is required")
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("sign: reading stdin: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(*secret))
	mac.Write(body)
	fmt.Printf("sha256=%s\n", hex.EncodeToString(mac.Sum(nil)))
}
