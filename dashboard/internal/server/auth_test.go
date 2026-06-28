package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp/totp"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/internal/db"
)

// --- fakes ---

// fakeStore is an in-memory Store for unit tests. It mirrors just enough of the
// real query semantics (case-insensitive username, single-use recovery codes,
// session expiry) to exercise the handlers without a database.
type fakeStore struct {
	mu         sync.Mutex
	users      map[int64]*db.User
	sessions   map[string]*db.Session
	recovery   map[int64]map[string]bool // userID -> codeHash -> used
	audit      []db.AppendAuditEntryParams
	deploys    []db.DeployHistory
	deployID   int64
	nextUserID int64
	failCreate bool
	// confirmErr, when set, is returned by ConfirmUserTOTP — used to simulate the
	// single-confirmed-admin unique-violation a concurrent redeem would trigger.
	confirmErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    map[int64]*db.User{},
		sessions: map[string]*db.Session{},
		recovery: map[int64]map[string]bool{},
	}
}

func (f *fakeStore) CountConfirmedAdmins(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, u := range f.users {
		if u.TotpConfirmedAt.Valid {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) GetUserByUsername(_ context.Context, username string) (db.GetUserByUsernameRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if strings.EqualFold(u.Username, username) {
			return db.GetUserByUsernameRow{
				ID:              u.ID,
				Username:        u.Username,
				PasswordHash:    u.PasswordHash,
				TotpSecret:      u.TotpSecret,
				TotpConfirmedAt: u.TotpConfirmedAt,
				FailedLogins:    u.FailedLogins,
				LockedUntil:     u.LockedUntil,
			}, nil
		}
	}
	return db.GetUserByUsernameRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetUserByID(_ context.Context, id int64) (db.GetUserByIDRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return db.GetUserByIDRow{}, pgx.ErrNoRows
	}
	return db.GetUserByIDRow{
		ID:              u.ID,
		Username:        u.Username,
		PasswordHash:    u.PasswordHash,
		TotpSecret:      u.TotpSecret,
		TotpConfirmedAt: u.TotpConfirmedAt,
		FailedLogins:    u.FailedLogins,
		LockedUntil:     u.LockedUntil,
	}, nil
}

func (f *fakeStore) ConfirmUserTOTP(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.confirmErr != nil {
		return f.confirmErr
	}
	u, ok := f.users[id]
	if !ok {
		return pgx.ErrNoRows
	}
	u.TotpConfirmedAt = pgtype.Timestamptz{Time: fixedNow(), Valid: true}
	return nil
}

func (f *fakeStore) IncrementFailedLogins(_ context.Context, id int64) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return 0, pgx.ErrNoRows
	}
	u.FailedLogins++
	return u.FailedLogins, nil
}

func (f *fakeStore) LockUser(_ context.Context, arg db.LockUserParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[arg.UserID]
	if !ok {
		return pgx.ErrNoRows
	}
	u.LockedUntil = arg.LockedUntil
	return nil
}

func (f *fakeStore) ResetFailedLogins(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return pgx.ErrNoRows
	}
	u.FailedLogins = 0
	u.LockedUntil = pgtype.Timestamptz{}
	return nil
}

func (f *fakeStore) CreateSession(_ context.Context, arg db.CreateSessionParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[arg.TokenHash] = &db.Session{
		TokenHash: arg.TokenHash,
		UserID:    arg.UserID,
		ExpiresAt: arg.ExpiresAt,
	}
	return nil
}

func (f *fakeStore) GetSession(_ context.Context, tokenHash string) (db.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[tokenHash]
	if !ok || (s.ExpiresAt.Valid && !s.ExpiresAt.Time.After(fixedNow())) {
		return db.Session{}, pgx.ErrNoRows
	}
	return *s, nil
}

func (f *fakeStore) TouchSession(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[tokenHash]; ok {
		s.LastUsedAt = pgtype.Timestamptz{Time: fixedNow(), Valid: true}
	}
	return nil
}

func (f *fakeStore) DeleteSession(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	return nil
}

func (f *fakeStore) MarkSessionReauth(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[tokenHash]; ok {
		s.ReauthAt = pgtype.Timestamptz{Time: fixedNow(), Valid: true}
	}
	return nil
}

func (f *fakeStore) ConsumeRecoveryCode(_ context.Context, arg db.ConsumeRecoveryCodeParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	codes := f.recovery[arg.UserID]
	used, ok := codes[arg.CodeHash]
	if !ok || used {
		return 0, pgx.ErrNoRows
	}
	codes[arg.CodeHash] = true
	return 1, nil
}

func (f *fakeStore) CountUnusedRecoveryCodes(_ context.Context, userID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, used := range f.recovery[userID] {
		if !used {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) AppendAuditEntry(_ context.Context, arg db.AppendAuditEntryParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, arg)
	return nil
}

func (f *fakeStore) CreateAdmin(_ context.Context, arg CreateAdminParams) (db.CreateUserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return db.CreateUserRow{}, errors.New("forced create failure")
	}
	// DeleteUnconfirmedUsers semantics.
	for id, u := range f.users {
		if !u.TotpConfirmedAt.Valid {
			delete(f.users, id)
			delete(f.recovery, id)
		}
	}
	f.nextUserID++
	id := f.nextUserID
	u := &db.User{
		ID:           id,
		Username:     arg.Username,
		PasswordHash: arg.PasswordHash,
		TotpSecret:   arg.SealedTOTPSecret,
	}
	f.users[id] = u
	f.recovery[id] = map[string]bool{}
	for _, h := range arg.RecoveryCodeHashes {
		f.recovery[id][h] = false
	}
	return db.CreateUserRow{ID: id, Username: arg.Username}, nil
}

func (f *fakeStore) confirmedUser(t *testing.T, username, password string) (int64, string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	secret, _, err := auth.GenerateTOTP(totpIssuer, username)
	if err != nil {
		t.Fatalf("gen totp: %v", err)
	}
	sealed, err := testCipherInstance.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	f.mu.Lock()
	f.nextUserID++
	id := f.nextUserID
	f.users[id] = &db.User{
		ID:              id,
		Username:        username,
		PasswordHash:    hash,
		TotpSecret:      sealed,
		TotpConfirmedAt: pgtype.Timestamptz{Time: fixedNow(), Valid: true},
	}
	f.recovery[id] = map[string]bool{}
	f.mu.Unlock()
	return id, secret
}

// --- clock + cipher ---

func fixedNow() time.Time {
	return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
}

// A single deterministic cipher instance shared by the test fakes so a sealed
// blob from a helper opens in a handler built by testServer.
var testCipherInstance = mustCipher()

func mustCipher() *auth.Cipher {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := auth.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		panic(err)
	}
	return c
}

func testCipher(t *testing.T) *auth.Cipher {
	t.Helper()
	return testCipherInstance
}

const testBootstrapToken = "install-token-xyz"

// --- harness ---

func authServer(t *testing.T, store *fakeStore) *Server {
	t.Helper()
	k8s := fake.NewClientBuilder().Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// post sends a JSON body to target, forwarding cookies, and returns the recorder
// (whose Result carries any Set-Cookie headers).
func post(t *testing.T, s *Server, target string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, rdr)
	req.RemoteAddr = "10.0.0.1:5555"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return m
}

// --- 1. status ---

func TestStatusStates(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	// needs_bootstrap with no admin.
	rec := getReq(t, s, "/api/auth/status")
	if got := decodeBody(t, rec)["state"]; got != "needs_bootstrap" {
		t.Fatalf("state = %v, want needs_bootstrap", got)
	}

	// needs_login once a confirmed admin exists.
	store.confirmedUser(t, "admin", "correct-horse-battery")
	rec = getReq(t, s, "/api/auth/status")
	if got := decodeBody(t, rec)["state"]; got != "needs_login" {
		t.Fatalf("state = %v, want needs_login", got)
	}

	// authenticated with a live session cookie.
	uid := store.firstUserID()
	raw := mustSession(t, store, uid)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := decodeBody(t, rec)
	if body["state"] != "authenticated" || body["username"] != "admin" {
		t.Fatalf("authenticated state = %v", body)
	}
}

func getReq(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.RemoteAddr = "10.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *fakeStore) firstUserID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.users {
		return id
	}
	return 0
}

func mustSession(t *testing.T, store *fakeStore, uid int64) string {
	t.Helper()
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	store.mu.Lock()
	store.sessions[hash] = &db.Session{
		TokenHash: hash,
		UserID:    uid,
		ExpiresAt: pgtype.Timestamptz{Time: fixedNow().Add(sessionTTL), Valid: true},
	}
	store.mu.Unlock()
	return raw
}

// --- 2. redeem ---

func TestRedeemHappyPath(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    testBootstrapToken,
		Username: "admin",
		Password: "correct-horse-battery",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["otpauthUrl"] == nil || !strings.Contains(body["otpauthUrl"].(string), "otpauth://") {
		t.Fatalf("missing otpauthUrl: %v", body)
	}
	codes, ok := body["recoveryCodes"].([]any)
	if !ok || len(codes) != recoveryCodeCount {
		t.Fatalf("recoveryCodes = %v, want %d", body["recoveryCodes"], recoveryCodeCount)
	}
	if c := cookieNamed(rec, challengeCookie); c == nil || !c.HttpOnly {
		t.Fatalf("challenge cookie missing or not HttpOnly: %+v", c)
	}
	// User created but not yet confirmed.
	if n, _ := store.CountConfirmedAdmins(context.Background()); n != 0 {
		t.Fatalf("confirmed admins = %d, want 0 before TOTP confirm", n)
	}
	// Audit recorded the success without leaking the password.
	assertAudited(t, store, "bootstrap.redeem", "success")
	for _, e := range store.audit {
		if strings.Contains(string(e.Detail), "correct-horse-battery") {
			t.Fatal("audit detail leaked the password")
		}
	}
}

func TestRedeemRefusedAfterBootstrap(t *testing.T) {
	store := newFakeStore()
	store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    testBootstrapToken,
		Username: "intruder",
		Password: "another-strong-pass",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("redeem-after-bootstrap status = %d, want 409", rec.Code)
	}
}

func TestRedeemWrongToken(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    "wrong-token",
		Username: "admin",
		Password: "correct-horse-battery",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", rec.Code)
	}
	if cookieNamed(rec, challengeCookie) != nil {
		t.Fatal("a rejected redeem must not set a challenge cookie")
	}
	assertAudited(t, store, "bootstrap.redeem", "failure")
}

func TestRedeemWeakPassword(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    testBootstrapToken,
		Username: "admin",
		Password: "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak-password status = %d, want 400", rec.Code)
	}
}

func TestRedeemStoreFailure(t *testing.T) {
	store := newFakeStore()
	store.failCreate = true
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    testBootstrapToken,
		Username: "admin",
		Password: "correct-horse-battery",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store-failure status = %d, want 500", rec.Code)
	}
	if cookieNamed(rec, challengeCookie) != nil {
		t.Fatal("a failed redeem must not set a challenge cookie")
	}
}

// --- 3. confirm TOTP ---

func TestConfirmTOTP(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	redeemRec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token:    testBootstrapToken,
		Username: "admin",
		Password: "correct-horse-battery",
	})
	challenge := cookieNamed(redeemRec, challengeCookie)
	uid := store.firstUserID()

	// Wrong code → 401, not confirmed, no session.
	bad := post(t, s, "/api/auth/totp/confirm", codeRequest{Code: "000000"}, challenge)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-code confirm status = %d, want 401", bad.Code)
	}
	if cookieNamed(bad, sessionCookie) != nil {
		t.Fatal("a failed confirm must not mint a session")
	}

	// Valid code → session minted, user confirmed.
	code := liveCode(t, store, uid)
	good := post(t, s, "/api/auth/totp/confirm", codeRequest{Code: code}, challenge)
	if good.Code != http.StatusOK {
		t.Fatalf("confirm status = %d (%s)", good.Code, good.Body.String())
	}
	if cookieNamed(good, sessionCookie) == nil {
		t.Fatal("confirm must mint a session cookie")
	}
	if n, _ := store.CountConfirmedAdmins(context.Background()); n != 1 {
		t.Fatalf("confirmed admins = %d, want 1", n)
	}
	assertAudited(t, store, "bootstrap.confirm_totp", "success")
}

// liveCode reads the stored sealed seed for a user and computes the current
// code. auth.ValidateTOTP (pquerna) validates against the real wall clock — the
// injected Now governs only session/lockout/challenge deadlines — so the code
// must be generated against time.Now(), not the fixed test clock.
func liveCode(t *testing.T, store *fakeStore, uid int64) string {
	t.Helper()
	store.mu.Lock()
	sealed := store.users[uid].TotpSecret
	store.mu.Unlock()
	secret, err := testCipherInstance.Open(sealed)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("gen code: %v", err)
	}
	return code
}

// --- 4. login (first factor) ---

func TestLoginWrongPassword(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "nope-nope-nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", rec.Code)
	}
	store.mu.Lock()
	failed := store.users[uid].FailedLogins
	store.mu.Unlock()
	if failed != 1 {
		t.Fatalf("failed_logins = %d, want 1", failed)
	}
}

func TestLoginLockout(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	for i := 0; i < maxFailedLogins; i++ {
		post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "wrong-and-long-x"})
	}
	store.mu.Lock()
	locked := store.users[uid].LockedUntil
	store.mu.Unlock()
	if !locked.Valid {
		t.Fatal("account should be locked after threshold failures")
	}

	// Even the correct password is refused with 423 while locked.
	rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	if rec.Code != http.StatusLocked {
		t.Fatalf("locked status = %d, want 423", rec.Code)
	}
	assertAudited(t, store, "auth.login", "locked")
}

func TestLoginCorrectPasswordThenTOTP(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	// First factor → totp_required + a challenge cookie.
	rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["state"] != "totp_required" {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	challenge := cookieNamed(rec, challengeCookie)
	if challenge == nil {
		t.Fatal("login must set a totp challenge cookie")
	}

	// Second factor with a live code → session, counter reset.
	code := liveCode(t, store, uid)
	rec2 := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: code}, challenge)
	if rec2.Code != http.StatusOK || decodeBody(t, rec2)["state"] != "authenticated" {
		t.Fatalf("login/totp status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if cookieNamed(rec2, sessionCookie) == nil {
		t.Fatal("login/totp must mint a session cookie")
	}
	store.mu.Lock()
	failed := store.users[uid].FailedLogins
	store.mu.Unlock()
	if failed != 0 {
		t.Fatalf("failed_logins = %d after success, want 0", failed)
	}
	assertAudited(t, store, "auth.login_totp", "success")
}

func TestLoginRecoveryCodeSingleUse(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	// Seed one recovery code.
	plain, hashes, err := auth.GenerateRecoveryCodes(1)
	if err != nil {
		t.Fatalf("gen recovery: %v", err)
	}
	store.mu.Lock()
	store.recovery[uid][hashes[0]] = false
	store.mu.Unlock()
	s := authServer(t, store)

	login := func() *http.Cookie {
		rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
		return cookieNamed(rec, challengeCookie)
	}

	// First use succeeds.
	rec := post(t, s, "/api/auth/login/totp", loginTOTPRequest{RecoveryCode: plain[0]}, login())
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery-code login status = %d (%s)", rec.Code, rec.Body.String())
	}
	// Replay is rejected (single-use).
	rec2 := post(t, s, "/api/auth/login/totp", loginTOTPRequest{RecoveryCode: plain[0]}, login())
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("recovery-code replay status = %d, want 401", rec2.Code)
	}
}

// --- 6. logout ---

func TestLogout(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)
	raw := mustSession(t, store, uid)

	rec := post(t, s, "/api/auth/logout", nil, &http.Cookie{Name: sessionCookie, Value: raw})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", rec.Code)
	}
	// Session deleted and cookie cleared.
	if _, err := store.GetSession(context.Background(), auth.HashToken(raw)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("logout must delete the session")
	}
	if c := cookieNamed(rec, sessionCookie); c == nil || c.MaxAge != -1 {
		t.Fatalf("logout must clear the cookie: %+v", c)
	}
}

// --- 7. stepup ---

func TestStepUp(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)
	raw := mustSession(t, store, uid)
	sessionCk := &http.Cookie{Name: sessionCookie, Value: raw}

	// A bad code is rejected.
	bad := post(t, s, "/api/auth/stepup", stepUpRequest{Code: "000000"}, sessionCk)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad stepup status = %d, want 401", bad.Code)
	}

	code := liveCode(t, store, uid)
	rec := post(t, s, "/api/auth/stepup", stepUpRequest{Code: code}, sessionCk)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stepup status = %d (%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	reauth := store.sessions[auth.HashToken(raw)].ReauthAt
	store.mu.Unlock()
	if !reauth.Valid {
		t.Fatal("stepup must stamp reauth_at on the session")
	}
}

// --- middleware ---

func TestRequireSessionRejects(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)
	guarded := s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	run := func(c *http.Cookie) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := run(nil); code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d, want 401", code)
	}
	if code := run(&http.Cookie{Name: sessionCookie, Value: "forged-token"}); code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d, want 401", code)
	}
	// Expired session.
	rawExp, hashExp, _ := auth.NewSessionToken()
	store.mu.Lock()
	store.sessions[hashExp] = &db.Session{TokenHash: hashExp, UserID: uid, ExpiresAt: pgtype.Timestamptz{Time: fixedNow().Add(-time.Hour), Valid: true}}
	store.mu.Unlock()
	if code := run(&http.Cookie{Name: sessionCookie, Value: rawExp}); code != http.StatusUnauthorized {
		t.Fatalf("expired cookie = %d, want 401", code)
	}
	// Valid session passes.
	raw := mustSession(t, store, uid)
	if code := run(&http.Cookie{Name: sessionCookie, Value: raw}); code != http.StatusOK {
		t.Fatalf("valid cookie = %d, want 200", code)
	}
}

func TestRequireStepUpFreshness(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)
	guarded := s.RequireStepUp(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	raw := mustSession(t, store, uid)
	hash := auth.HashToken(raw)

	run := func() int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}

	// No reauth yet → 403.
	if code := run(); code != http.StatusForbidden {
		t.Fatalf("stale stepup = %d, want 403", code)
	}
	// Stale reauth → 403.
	store.mu.Lock()
	store.sessions[hash].ReauthAt = pgtype.Timestamptz{Time: fixedNow().Add(-stepUpFreshness - time.Minute), Valid: true}
	store.mu.Unlock()
	if code := run(); code != http.StatusForbidden {
		t.Fatalf("expired reauth = %d, want 403", code)
	}
	// Fresh reauth → 200.
	store.mu.Lock()
	store.sessions[hash].ReauthAt = pgtype.Timestamptz{Time: fixedNow(), Valid: true}
	store.mu.Unlock()
	if code := run(); code != http.StatusOK {
		t.Fatalf("fresh reauth = %d, want 200", code)
	}
}

// --- rate limit ---

func TestRateLimiter(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	// A credential endpoint is rate-limited: past the window it returns 429.
	var last int
	for i := 0; i < rateLimitMax+1; i++ {
		rec := post(t, s, "/api/auth/login", loginRequest{Username: "nobody", Password: "x"})
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("request past the window = %d, want 429", last)
	}
}

// TestStatusNotRateLimited proves GET /api/auth/status is exempt from the limiter
// so a SPA polling it never 429s itself (it lives outside the credential group).
func TestStatusNotRateLimited(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	for i := 0; i < rateLimitMax*2; i++ {
		rec := getReq(t, s, "/api/auth/status")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("status endpoint was rate-limited at iteration %d", i)
		}
	}
}

// TestRateLimiterWindowAndIsolation drives newRateLimiter directly with a
// controllable clock: it resets after the window elapses and tracks each IP
// independently.
func TestRateLimiterWindowAndIsolation(t *testing.T) {
	now := fixedNow()
	clock := func() time.Time { return now }
	rl := newRateLimiter(3, time.Minute, clock)

	const ipA, ipB = "10.0.0.1", "10.0.0.2"

	// Exhaust ipA's budget.
	for i := 0; i < 3; i++ {
		if !rl.allow(ipA) {
			t.Fatalf("ipA request %d should be allowed", i)
		}
	}
	if rl.allow(ipA) {
		t.Fatal("ipA should be over budget within the window")
	}

	// A second IP is independent — not affected by ipA's exhaustion.
	if !rl.allow(ipB) {
		t.Fatal("ipB should be allowed while ipA is limited (per-IP isolation)")
	}

	// Advance past the window: ipA's window resets and allow() returns true again.
	now = now.Add(time.Minute + time.Second)
	if !rl.allow(ipA) {
		t.Fatal("ipA should be allowed again after the window elapses")
	}
}

// --- M2.3 security-property pins ---

// sealedChallenge builds a challenge cookie sealed with the test cipher so a test
// can present an arbitrary (stage, expiry) — used to exercise stage-confusion and
// the expiry boundary directly.
func sealedChallenge(t *testing.T, uid int64, stage string, expires time.Time) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(challenge{UID: uid, Stage: stage, Expires: expires.Unix()})
	if err != nil {
		t.Fatalf("marshal challenge: %v", err)
	}
	sealed, err := testCipherInstance.Seal(string(payload))
	if err != nil {
		t.Fatalf("seal challenge: %v", err)
	}
	return &http.Cookie{Name: challengeCookie, Value: sealed}
}

// TestLoginTOTPWrongCodeIncrementsLockout proves the second-factor failure path
// counts toward lockout (handleLoginTOTP calls recordFailedLogin).
func TestLoginTOTPWrongCodeIncrementsLockout(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	ch := cookieNamed(rec, challengeCookie)
	if ch == nil {
		t.Fatal("login must set a totp challenge cookie")
	}
	bad := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: "000000"}, ch)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong totp code status = %d, want 401", bad.Code)
	}
	store.mu.Lock()
	failed := store.users[uid].FailedLogins
	store.mu.Unlock()
	if failed != 1 {
		t.Fatalf("failed_logins = %d after a wrong second factor, want 1", failed)
	}
}

// TestChallengeStageConfusion proves a challenge minted for one stage cannot be
// replayed against the other consuming endpoint.
func TestChallengeStageConfusion(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	// An "enroll"-stage cookie presented to login/totp (which expects "totp").
	enrollCk := sealedChallenge(t, uid, stageEnroll, fixedNow().Add(time.Minute))
	rec := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: "000000"}, enrollCk)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("enroll-stage cookie at login/totp = %d, want 401", rec.Code)
	}

	// A "totp"-stage cookie presented to totp/confirm (which expects "enroll").
	totpCk := sealedChallenge(t, uid, stageTOTP, fixedNow().Add(time.Minute))
	rec = post(t, s, "/api/auth/totp/confirm", codeRequest{Code: "000000"}, totpCk)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("totp-stage cookie at totp/confirm = %d, want 401", rec.Code)
	}
}

// TestTamperedChallengeCookie proves a corrupted sealed value fails the AEAD open
// at both consuming endpoints.
func TestTamperedChallengeCookie(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	tamper := func(stage string) *http.Cookie {
		c := sealedChallenge(t, uid, stage, fixedNow().Add(time.Minute))
		// Flip the last character of the sealed value to corrupt the ciphertext.
		v := []byte(c.Value)
		if v[len(v)-1] == 'A' {
			v[len(v)-1] = 'B'
		} else {
			v[len(v)-1] = 'A'
		}
		c.Value = string(v)
		return c
	}

	if rec := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: "000000"}, tamper(stageTOTP)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie at login/totp = %d, want 401", rec.Code)
	}
	if rec := post(t, s, "/api/auth/totp/confirm", codeRequest{Code: "000000"}, tamper(stageEnroll)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie at totp/confirm = %d, want 401", rec.Code)
	}
}

// TestExpiredChallengeCookie proves readChallenge rejects a challenge at/after its
// expiry second (the >= boundary), at both consuming endpoints.
func TestExpiredChallengeCookie(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	// Expires one second before fixedNow → definitely expired.
	expired := func(stage string) *http.Cookie {
		return sealedChallenge(t, uid, stage, fixedNow().Add(-time.Second))
	}
	if rec := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: "000000"}, expired(stageTOTP)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie at login/totp = %d, want 401", rec.Code)
	}
	if rec := post(t, s, "/api/auth/totp/confirm", codeRequest{Code: "000000"}, expired(stageEnroll)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie at totp/confirm = %d, want 401", rec.Code)
	}
}

// TestLoginAutoUnlockAfterWindow proves an elapsed lock no longer 423s: a wrong
// password on a user whose LockedUntil is in the past returns 401 (not 423).
func TestLoginAutoUnlockAfterWindow(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	store.mu.Lock()
	store.users[uid].LockedUntil = pgtype.Timestamptz{Time: fixedNow().Add(-time.Second), Valid: true}
	store.mu.Unlock()
	s := authServer(t, store)

	rec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "wrong-and-long-x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login after lock elapsed = %d, want 401 (not 423)", rec.Code)
	}
	if body := decodeBody(t, rec)["error"]; body != "invalid_credentials" {
		t.Fatalf("error = %v, want invalid_credentials", body)
	}
}

// TestLoginUnknownUserParity proves a non-existent username returns the same
// generic 401 as a wrong password (no user enumeration) AND that
// recordFailedLogin is never called (no row exists to count).
func TestLoginUnknownUserParity(t *testing.T) {
	store := newFakeStore()
	store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	unknown := post(t, s, "/api/auth/login", loginRequest{Username: "ghost", Password: "whatever-long-x"})
	wrong := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "whatever-long-x"})

	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("unknown=%d wrong=%d, both want 401", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("bodies differ — user enumeration: unknown=%q wrong=%q", unknown.Body.String(), wrong.Body.String())
	}
	// The unknown user has no row, so its failed-login counter could only be
	// observed if recordFailedLogin had somehow run against the existing admin.
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, u := range store.users {
		if u.Username == "admin" && u.FailedLogins != 1 {
			t.Fatalf("admin failed_logins = %d, want 1 (only the wrong-password attempt counts)", u.FailedLogins)
		}
	}
}

// TestUnconfirmedUserNotLocked proves the bootstrap-DoS fix: an unconfirmed user
// (mid-enrollment) never has failed_logins incremented or gets locked, even on
// repeated wrong-password attempts, AND a correct password on an unconfirmed user
// still returns the generic 401.
func TestUnconfirmedUserNotLocked(t *testing.T) {
	store := newFakeStore()
	srv := authServer(t, store)
	// CreateAdmin (via redeem) makes an unconfirmed user (TOTP not yet confirmed).
	redeemRec := post(t, srv, "/api/auth/redeem", redeemRequest{
		Token: testBootstrapToken, Username: "admin", Password: "correct-horse-battery",
	})
	if redeemRec.Code != http.StatusOK {
		t.Fatalf("redeem status = %d", redeemRec.Code)
	}
	uid := store.firstUserID()

	for i := 0; i < maxFailedLogins+2; i++ {
		post(t, srv, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	}
	store.mu.Lock()
	u := store.users[uid]
	failed, locked := u.FailedLogins, u.LockedUntil.Valid
	store.mu.Unlock()
	if failed != 0 {
		t.Fatalf("unconfirmed user failed_logins = %d, want 0 (must never count)", failed)
	}
	if locked {
		t.Fatal("unconfirmed user must never be locked (bootstrap DoS)")
	}

	// Even the correct password on the unconfirmed user is a generic 401.
	rec := post(t, srv, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("correct password on unconfirmed user = %d, want 401", rec.Code)
	}
}

// TestConfirmTOTPConcurrentRedeemLoses proves a 23505 from ConfirmUserTOTP (the
// single-confirmed-admin index losing the race) maps to a clean 409, not a 500.
func TestConfirmTOTPConcurrentRedeemLoses(t *testing.T) {
	store := newFakeStore()
	s := authServer(t, store)

	redeemRec := post(t, s, "/api/auth/redeem", redeemRequest{
		Token: testBootstrapToken, Username: "admin", Password: "correct-horse-battery",
	})
	ch := cookieNamed(redeemRec, challengeCookie)
	uid := store.firstUserID()
	store.confirmErr = &pgconn.PgError{Code: "23505"}

	rec := post(t, s, "/api/auth/totp/confirm", codeRequest{Code: liveCode(t, store, uid)}, ch)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm losing the race = %d, want 409", rec.Code)
	}
	if body := decodeBody(t, rec)["error"]; body != "already_bootstrapped" {
		t.Fatalf("error = %v, want already_bootstrapped", body)
	}
}

// TestHappyPathCookiesAreSameSiteStrict pins SameSite=Strict on both the challenge
// and session cookies the success paths set.
func TestHappyPathCookiesAreSameSiteStrict(t *testing.T) {
	store := newFakeStore()
	uid, _ := store.confirmedUser(t, "admin", "correct-horse-battery")
	s := authServer(t, store)

	loginRec := post(t, s, "/api/auth/login", loginRequest{Username: "admin", Password: "correct-horse-battery"})
	ch := cookieNamed(loginRec, challengeCookie)
	if ch == nil || ch.SameSite != http.SameSiteStrictMode {
		t.Fatalf("challenge cookie SameSite = %v, want Strict", samesite(ch))
	}

	code := liveCode(t, store, uid)
	totpRec := post(t, s, "/api/auth/login/totp", loginTOTPRequest{Code: code}, ch)
	sess := cookieNamed(totpRec, sessionCookie)
	if sess == nil || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie SameSite = %v, want Strict", samesite(sess))
	}
}

func samesite(c *http.Cookie) any {
	if c == nil {
		return "<nil cookie>"
	}
	return c.SameSite
}

// --- helpers ---

func assertAudited(t *testing.T, store *fakeStore, action, outcome string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, e := range store.audit {
		if e.Action == action && e.Outcome == outcome {
			return
		}
	}
	t.Fatalf("no audit entry for action=%q outcome=%q (have %+v)", action, outcome, store.audit)
}
