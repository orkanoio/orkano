package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/dashboard/internal/oidc"
	"github.com/orkanoio/orkano/internal/db"
)

// fakeOIDC drives the handler flow without a live IdP. It records the values the
// login handler minted so the callback test can replay the matching state.
type fakeOIDC struct {
	identity    *oidc.Identity
	exchangeErr error
	allow       bool
	lastState   string
	lastNonce   string
	lastReauth  bool
}

func (f *fakeOIDC) AuthCodeURL(state, nonce, _ string, reauth bool) string {
	f.lastState, f.lastNonce, f.lastReauth = state, nonce, reauth
	return "https://idp.example/authorize?state=" + url.QueryEscape(state)
}

func (f *fakeOIDC) Exchange(_ context.Context, _, _, _ string) (*oidc.Identity, error) {
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.identity, nil
}

func (f *fakeOIDC) Authorize(*oidc.Identity) bool { return f.allow }

func oidcServer(t *testing.T, store *fakeStore, oa OIDCAuthenticator) *Server {
	t.Helper()
	k8s := fakeclient.NewClientBuilder().Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
		OIDC:               oa,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// getWithCookies is a GET that forwards cookies (the no-cookie getReq lives in
// auth_test.go; cookieNamed and decodeBody are shared from there too).
func getWithCookies(t *testing.T, s *Server, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// runOIDCCallback drives login → callback with a verified identity and returns
// the callback recorder.
func runOIDCCallback(t *testing.T, s *Server, fake *fakeOIDC) *httptest.ResponseRecorder {
	t.Helper()
	login := getReq(t, s, "/api/auth/oidc/login")
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", login.Code)
	}
	flowCk := cookieNamed(login, oidcCookie)
	if flowCk == nil {
		t.Fatal("login set no flow cookie")
	}
	target := "/api/auth/oidc/callback?code=the-code&state=" + url.QueryEscape(fake.lastState)
	return getWithCookies(t, s, target, flowCk)
}

func seedOIDCUser(store *fakeStore, uid int64, username, issuer, subject string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.users[uid] = &db.User{
		ID:          uid,
		Username:    username,
		OidcIssuer:  pgtype.Text{String: issuer, Valid: true},
		OidcSubject: pgtype.Text{String: subject, Valid: true},
	}
	if uid >= store.nextUserID {
		store.nextUserID = uid
	}
}

func TestOIDCStatusReportsEnabled(t *testing.T) {
	t.Run("disabled when no authenticator", func(t *testing.T) {
		s := authServer(t, newFakeStore()) // no OIDC
		rec := getReq(t, s, "/api/auth/status")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["oidcEnabled"] != false {
			t.Fatalf("oidcEnabled = %v, want false", body["oidcEnabled"])
		}
	})
	t.Run("enabled when wired", func(t *testing.T) {
		s := oidcServer(t, newFakeStore(), &fakeOIDC{})
		rec := getReq(t, s, "/api/auth/status")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["oidcEnabled"] != true {
			t.Fatalf("oidcEnabled = %v, want true", body["oidcEnabled"])
		}
	})
}

func TestOIDCLoginRedirectsAndSetsCookie(t *testing.T) {
	fake := &fakeOIDC{}
	s := oidcServer(t, newFakeStore(), fake)
	rec := getReq(t, s, "/api/auth/oidc/login")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Result().Header.Get("Location"); !strings.HasPrefix(loc, "https://idp.example/authorize") {
		t.Fatalf("redirect location = %q", loc)
	}
	if fake.lastReauth {
		t.Fatal("a plain login must not request prompt=login (that is step-up)")
	}
	ck := cookieNamed(rec, oidcCookie)
	if ck == nil {
		t.Fatal("no flow cookie set")
	}
	// SameSite=Lax is load-bearing — Strict would drop on the cross-site callback.
	if ck.SameSite != http.SameSiteLaxMode || !ck.HttpOnly {
		t.Fatalf("flow cookie attrs: SameSite=%v HttpOnly=%v", ck.SameSite, ck.HttpOnly)
	}
}

func TestOIDCLoginDisabledRedirects(t *testing.T) {
	s := authServer(t, newFakeStore()) // OIDC nil
	rec := getReq(t, s, "/api/auth/oidc/login")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Result().Header.Get("Location"), ssoDisabled) {
		t.Fatalf("disabled login: status %d loc %q", rec.Code, rec.Result().Header.Get("Location"))
	}
	if cookieNamed(rec, oidcCookie) != nil {
		t.Fatal("disabled login must not set a flow cookie")
	}
}

func TestOIDCCallbackProvisionsAndAuthenticates(t *testing.T) {
	store := newFakeStore()
	fake := &fakeOIDC{
		allow:    true,
		identity: &oidc.Identity{Subject: "sub-1", Issuer: "https://idp.example", Email: "alice@example.com", EmailVerified: true},
	}
	s := oidcServer(t, store, fake)

	rec := runOIDCCallback(t, s, fake)
	if rec.Code != http.StatusFound || rec.Result().Header.Get("Location") != "/" {
		t.Fatalf("callback: status %d loc %q", rec.Code, rec.Result().Header.Get("Location"))
	}
	sess := cookieNamed(rec, sessionCookie)
	if sess == nil || sess.Value == "" {
		t.Fatal("callback minted no session cookie")
	}
	// A JIT user exists, keyed on (issuer, subject), username = the IdP email.
	u, err := store.GetUserByOIDC(context.Background(), db.GetUserByOIDCParams{
		Issuer: pgText("https://idp.example"), Subject: pgText("sub-1"),
	})
	if err != nil || u.Username != "alice@example.com" {
		t.Fatalf("JIT user: %+v, err %v", u, err)
	}

	// The minted OIDC session is admitted by resolveSession (the relaxation) and
	// reports oidc:true so the SPA picks the OIDC step-up path.
	st := getWithCookies(t, s, "/api/auth/status", sess)
	var body map[string]any
	_ = json.Unmarshal(st.Body.Bytes(), &body)
	if body["state"] != "authenticated" || body["oidc"] != true {
		t.Fatalf("status after OIDC login: %v", body)
	}
}

func TestOIDCCallbackReusesExistingUser(t *testing.T) {
	store := newFakeStore()
	seedOIDCUser(store, 7, "alice@example.com", "https://idp.example", "sub-1")
	fake := &fakeOIDC{
		allow:    true,
		identity: &oidc.Identity{Subject: "sub-1", Issuer: "https://idp.example", Email: "alice@example.com", EmailVerified: true},
	}
	s := oidcServer(t, store, fake)

	rec := runOIDCCallback(t, s, fake)
	if rec.Code != http.StatusFound || rec.Result().Header.Get("Location") != "/" {
		t.Fatalf("callback: status %d", rec.Code)
	}
	if len(store.users) != 1 {
		t.Fatalf("expected no new user (reuse), have %d", len(store.users))
	}
}

func TestOIDCCallbackFailures(t *testing.T) {
	identity := &oidc.Identity{Subject: "sub-1", Issuer: "https://idp.example", Email: "mallory@example.com", EmailVerified: true}

	t.Run("not on the allowlist", func(t *testing.T) {
		store := newFakeStore()
		fake := &fakeOIDC{allow: false, identity: identity}
		s := oidcServer(t, store, fake)
		rec := runOIDCCallback(t, s, fake)
		assertSSOError(t, rec, ssoNotAllowed)
		if cookieNamed(rec, sessionCookie) != nil {
			t.Fatal("a denied identity must get no session")
		}
		if len(store.users) != 0 {
			t.Fatal("a denied identity must not be provisioned")
		}
	})

	t.Run("exchange error", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity, exchangeErr: errors.New("bad code")}
		s := oidcServer(t, newFakeStore(), fake)
		assertSSOError(t, runOIDCCallback(t, s, fake), ssoExchange)
	})

	t.Run("state mismatch", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		login := getReq(t, s, "/api/auth/oidc/login")
		flowCk := cookieNamed(login, oidcCookie)
		// Replay the callback with a forged state, the real cookie.
		rec := getWithCookies(t, s, "/api/auth/oidc/callback?code=x&state=forged", flowCk)
		assertSSOError(t, rec, ssoStateMismatch)
	})

	t.Run("no flow cookie", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		rec := getReq(t, s, "/api/auth/oidc/callback?code=x&state=whatever")
		assertSSOError(t, rec, ssoNoFlow)
	})

	t.Run("idp-reported error", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		login := getReq(t, s, "/api/auth/oidc/login")
		flowCk := cookieNamed(login, oidcCookie)
		rec := getWithCookies(t, s, "/api/auth/oidc/callback?error=access_denied&state="+url.QueryEscape(fake.lastState), flowCk)
		assertSSOError(t, rec, ssoIdP)
	})
}

// sealedOIDCFlow seals a flow value into a cookie with the shared test cipher, so
// a test can craft an expired or otherwise specific flow cookie directly.
func sealedOIDCFlow(t *testing.T, flow oidcFlow) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(flow)
	if err != nil {
		t.Fatalf("marshal flow: %v", err)
	}
	sealed, err := testCipherInstance.Seal(string(payload))
	if err != nil {
		t.Fatalf("seal flow: %v", err)
	}
	return &http.Cookie{Name: oidcCookie, Value: sealed}
}

func TestOIDCCallbackCookieIntegrity(t *testing.T) {
	identity := &oidc.Identity{Subject: "sub-1", Issuer: "https://idp.example", Email: "alice@example.com", EmailVerified: true}

	t.Run("tampered flow cookie is rejected", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		login := getReq(t, s, "/api/auth/oidc/login")
		flowCk := cookieNamed(login, oidcCookie)
		flowCk.Value += "tampered" // corrupt the AEAD blob
		rec := getWithCookies(t, s, "/api/auth/oidc/callback?code=x&state="+url.QueryEscape(fake.lastState), flowCk)
		assertSSOError(t, rec, ssoNoFlow)
	})

	t.Run("expired flow cookie is rejected", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		expired := sealedOIDCFlow(t, oidcFlow{
			State: "s", Nonce: "n", Verifier: "v", Expires: fixedNow().Add(-time.Second).Unix(),
		})
		rec := getWithCookies(t, s, "/api/auth/oidc/callback?code=x&state=s", expired)
		assertSSOError(t, rec, ssoNoFlow)
	})

	t.Run("a successful callback clears the flow cookie", func(t *testing.T) {
		fake := &fakeOIDC{allow: true, identity: identity}
		s := oidcServer(t, newFakeStore(), fake)
		rec := runOIDCCallback(t, s, fake)
		cleared := cookieNamed(rec, oidcCookie)
		if cleared == nil || cleared.MaxAge >= 0 {
			t.Fatalf("callback must expire the flow cookie, got %+v", cleared)
		}
	})
}

func assertSSOError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Result().Header.Get("Location")
	if loc != "/?sso_error="+code {
		t.Fatalf("redirect = %q, want /?sso_error=%s", loc, code)
	}
}

func TestOIDCUserCannotPasswordLogin(t *testing.T) {
	store := newFakeStore()
	seedOIDCUser(store, 9, "alice@example.com", "https://idp.example", "sub-1")
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/login", loginRequest{Username: "alice@example.com", Password: "anything"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_credentials" {
		t.Fatalf("error = %q, want invalid_credentials (no totp challenge)", body["error"])
	}
	if cookieNamed(rec, challengeCookie) != nil {
		t.Fatal("an OIDC username must not advance to the TOTP step")
	}
}

func TestOIDCSessionRefusedAtPasswordStepUp(t *testing.T) {
	store := newFakeStore()
	seedOIDCUser(store, 11, "alice@example.com", "https://idp.example", "sub-1")
	raw := mustSession(t, store, 11)
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/stepup", stepUpRequest{Code: "123456"},
		&http.Cookie{Name: sessionCookie, Value: raw})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "oidc_stepup_required" {
		t.Fatalf("error = %q, want oidc_stepup_required", body["error"])
	}
}
