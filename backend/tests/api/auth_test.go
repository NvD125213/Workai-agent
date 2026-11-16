package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"notification-job-mb/internal/handlers"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/routes"
	"notification-job-mb/internal/services"
)

const testSecret = "test-secret-at-least-32-bytes-long!!"

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeUsers struct{ byID map[uint]*models.User }

func (f *fakeUsers) FindByEmail(_ context.Context, email string) (*models.User, error) {
	for _, u := range f.byID {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, repositories.ErrNotFound
}

func (f *fakeUsers) FindByID(_ context.Context, id uint) (*models.User, error) {
	if u, ok := f.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, repositories.ErrNotFound
}

type fakeTokens struct {
	mu     sync.Mutex
	byHash map[string]*models.RefreshToken
}

func (f *fakeTokens) Create(_ context.Context, t *models.RefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.byHash[t.TokenHash] = &cp
	return nil
}

func (f *fakeTokens) Consume(_ context.Context, hash string, now time.Time) (*models.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byHash[hash]
	if !ok {
		return nil, repositories.ErrNotFound
	}
	if t.RevokedAt != nil {
		cp := *t
		return &cp, repositories.ErrTokenAlreadyRevoked
	}
	if !t.ExpiresAt.After(now) {
		return nil, repositories.ErrNotFound
	}
	t.RevokedAt = &now
	cp := *t
	return &cp, nil
}

func (f *fakeTokens) Revoke(_ context.Context, hash string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.byHash[hash]; ok && t.RevokedAt == nil {
		t.RevokedAt = &now
	}
	return nil
}

func (f *fakeTokens) RevokeAllForUser(_ context.Context, userID uint, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byHash {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &now
		}
	}
	return nil
}

// ── harness ─────────────────────────────────────────────────────────────────

type harness struct {
	engine *gin.Engine
	tokens *fakeTokens
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	passwords := services.NewPasswordService()
	hash, err := passwords.Hash("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	users := &fakeUsers{byID: map[uint]*models.User{
		1: {ID: 1, Email: "user@example.com", Name: "User", PasswordHash: hash, Role: models.RoleUser, IsActive: true},
		2: {ID: 2, Email: "disabled@example.com", Name: "Disabled", PasswordHash: hash, Role: models.RoleUser, IsActive: false},
	}}
	tokens := &fakeTokens{byHash: map[string]*models.RefreshToken{}}

	tokenService, err := services.NewTokenService(services.TokenConfig{
		AccessSecret: testSecret,
		Issuer:       "test",
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	authService, err := services.NewAuthService(users, tokens, passwords, tokenService)
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	routes.Register(engine, routes.Dependencies{
		Health:        handlers.NewHealthHandler(nil),
		Auth:          handlers.NewAuthHandler(authService),
		TokenVerifier: tokenService,
	})
	return &harness{engine: engine, tokens: tokens}
}

func (h *harness) do(method, path, body, bearer string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.engine.ServeHTTP(w, r)
	return w
}

func (h *harness) login(t *testing.T) handlers.AuthResponse {
	t.Helper()
	w := h.do(http.MethodPost, "/api/v1/auth/login", `{"email":"  User@Example.com ","password":"correct-password"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body)
	}
	var res handlers.AuthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body, err)
	}
	return body.Error.Code
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestLoginSuccessThenMe(t *testing.T) {
	h := newHarness(t)
	res := h.login(t)

	if res.AccessToken == "" || res.RefreshToken == "" || res.TokenType != "Bearer" || res.ExpiresIn != 900 {
		t.Fatalf("unexpected auth response: %+v", res)
	}
	if res.User.ID != 1 || res.User.Email != "user@example.com" {
		t.Fatalf("unexpected user: %+v", res.User)
	}

	stored := 0
	for hash := range h.tokens.byHash {
		if hash == res.RefreshToken {
			t.Fatal("raw refresh token must not be stored")
		}
		stored++
	}
	if stored != 1 {
		t.Fatalf("stored refresh tokens = %d, want 1", stored)
	}

	w := h.do(http.MethodGet, "/api/v1/auth/me", "", res.AccessToken)
	if w.Code != http.StatusOK {
		t.Fatalf("me status = %d, body = %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "password") {
		t.Fatalf("me response leaks password field: %s", w.Body)
	}
}

func TestLoginRejections(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"wrong password", `{"email":"user@example.com","password":"nope"}`, 401, "invalid_credentials"},
		{"unknown email", `{"email":"ghost@example.com","password":"correct-password"}`, 401, "invalid_credentials"},
		{"disabled account", `{"email":"disabled@example.com","password":"correct-password"}`, 403, "account_disabled"},
		{"missing password", `{"email":"user@example.com"}`, 400, "invalid_request"},
		{"malformed email", `{"email":"not-an-email","password":"x"}`, 401, "invalid_credentials"},
		{"malformed json", `{"email":`, 400, "invalid_request"},
		{"oversized body", `{"email":"user@example.com","password":"` + strings.Repeat("a", 8<<10) + `"}`, 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := h.do(http.MethodPost, "/api/v1/auth/login", tt.body, "")
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, tt.wantCode, w.Body)
			}
			if got := errorCode(t, w); got != tt.wantErr {
				t.Fatalf("error code = %q, want %q", got, tt.wantErr)
			}
		})
	}
	if len(h.tokens.byHash) != 0 {
		t.Fatalf("rejected logins issued %d refresh tokens", len(h.tokens.byHash))
	}
}

func TestMeRejectsBadTokens(t *testing.T) {
	h := newHarness(t)
	now := time.Now()

	sign := func(method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := jwt.MapClaims{"sub": "1", "iss": "test", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}

	tests := []struct{ name, token string }{
		{"no token", ""},
		{"garbage", "abc.def.ghi"},
		{"wrong secret", sign(jwt.SigningMethodHS256, []byte("another-secret-that-is-32-bytes-long"), valid)},
		{"alg none", sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid)},
		{"expired", sign(jwt.SigningMethodHS256, []byte(testSecret), jwt.MapClaims{"sub": "1", "iss": "test", "iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()})},
		{"wrong issuer", sign(jwt.SigningMethodHS256, []byte(testSecret), jwt.MapClaims{"sub": "1", "iss": "evil", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})},
		{"no expiry", sign(jwt.SigningMethodHS256, []byte(testSecret), jwt.MapClaims{"sub": "1", "iss": "test", "iat": now.Unix()})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := h.do(http.MethodGet, "/api/v1/auth/me", "", tt.token)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body = %s", w.Code, w.Body)
			}
		})
	}
}

func TestRefreshRotatesAndDetectsReuse(t *testing.T) {
	h := newHarness(t)
	first := h.login(t)

	refresh := func(token string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"refresh_token": token})
		return h.do(http.MethodPost, "/api/v1/auth/refresh", string(body), "")
	}

	w := refresh(first.RefreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", w.Code, w.Body)
	}
	var second handlers.AuthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}

	// Replaying the rotated token is rejected and revokes the whole family.
	if w := refresh(first.RefreshToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", w.Code)
	}
	if w := refresh(second.RefreshToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("after reuse detection, newest token status = %d, want 401", w.Code)
	}

	if w := refresh("unknown-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d, want 401", w.Code)
	}
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	h := newHarness(t)
	res := h.login(t)
	body, _ := json.Marshal(map[string]string{"refresh_token": res.RefreshToken})

	for i := range 2 { // idempotent
		w := h.do(http.MethodPost, "/api/v1/auth/logout", string(body), "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("logout #%d status = %d, body = %s", i+1, w.Code, w.Body)
		}
	}
	w := h.do(http.MethodPost, "/api/v1/auth/refresh", string(body), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout status = %d, want 401", w.Code)
	}
}
