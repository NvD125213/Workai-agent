package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/handlers"
	"notification-job-mb/internal/mailer"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/routes"
	"notification-job-mb/internal/services"
)

// fakeResetStore models only what the HTTP contract depends on.
type fakeResetStore struct {
	issued    int
	discarded []uint
	codeOK    bool            // whether Verify accepts the code
	tokens    map[string]uint // live reset token hash -> user id
	redeemed  bool
}

func (f *fakeResetStore) Issue(_ context.Context, otp *models.PasswordResetOTP, _ time.Time, _ repositories.ResetLimits) error {
	f.issued++
	otp.ID = uint(f.issued)
	return nil
}

func (f *fakeResetStore) Discard(_ context.Context, id uint) error {
	f.discarded = append(f.discarded, id)
	return nil
}

type fakeQueue struct {
	err  error
	msgs []mailer.Message
}

func (f *fakeQueue) Enqueue(msg mailer.Message, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msg)
	return nil
}

func (f *fakeResetStore) Verify(_ context.Context, userID uint, _ string, _ time.Time, _ int, token repositories.ResetToken) error {
	if !f.codeOK {
		return repositories.ErrResetCodeInvalid
	}
	f.codeOK = false // a code works once
	f.tokens[token.Hash] = userID
	return nil
}

func (f *fakeResetStore) Redeem(_ context.Context, tokenHash string, _ time.Time, newHash func() (string, error)) error {
	if _, ok := f.tokens[tokenHash]; !ok {
		return repositories.ErrResetTokenInvalid
	}
	if _, err := newHash(); err != nil {
		return err
	}
	delete(f.tokens, tokenHash)
	f.redeemed = true
	return nil
}

func newResetEngine(t *testing.T) (*gin.Engine, *fakeResetStore) {
	engine, store, _ := newResetEngineWithQueue(t, &fakeQueue{})
	return engine, store
}

func newResetEngineWithQueue(t *testing.T, queue *fakeQueue) (*gin.Engine, *fakeResetStore, *fakeQueue) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	users := &fakeUsers{byID: map[uint]*models.User{
		1: {ID: 1, Email: "user@example.com", Name: "User", Role: models.RoleUser, IsActive: true},
		2: {ID: 2, Email: "disabled@example.com", Name: "D", Role: models.RoleUser, IsActive: false},
	}}
	store := &fakeResetStore{codeOK: true, tokens: map[string]uint{}}
	svc, err := services.NewPasswordResetService(users, store, queue, services.NewPasswordService(), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	tokenService, err := services.NewTokenService(services.TokenConfig{
		AccessSecret: testSecret, Issuer: "test", AccessTTL: time.Minute, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	routes.Register(engine, routes.Dependencies{
		Health:        handlers.NewHealthHandler(nil),
		Auth:          handlers.NewAuthHandler(nil),
		PasswordReset: handlers.NewPasswordResetHandler(svc),
		TokenVerifier: tokenService,
	})
	return engine, store, queue
}

func TestForgotPasswordQueuesOTPEmail(t *testing.T) {
	engine, _, queue := newResetEngineWithQueue(t, &fakeQueue{})
	if w := post(engine, "/api/v1/auth/forgot-password", `{"email":"user@example.com"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	if len(queue.msgs) != 1 {
		t.Fatalf("queued %d emails, want 1", len(queue.msgs))
	}
	msg := queue.msgs[0]
	if msg.To != "user@example.com" || msg.ID != "password-reset-1" {
		t.Errorf("unexpected message %+v", msg)
	}
	if !regexp.MustCompile(`\b\d{6}\b`).MatchString(msg.Body) {
		t.Errorf("body has no 6-digit code: %q", msg.Body)
	}
}

func TestForgotPasswordQueueFullDiscardsCodeAndHidesIt(t *testing.T) {
	engine, store, _ := newResetEngineWithQueue(t, &fakeQueue{err: mailer.ErrQueueFull})
	full := post(engine, "/api/v1/auth/forgot-password", `{"email":"user@example.com"}`)
	unknown := post(engine, "/api/v1/auth/forgot-password", `{"email":"nobody@example.com"}`)

	if full.Code != http.StatusAccepted || full.Body.String() != unknown.Body.String() {
		t.Fatalf("overload must look like any other request: %d %s", full.Code, full.Body.String())
	}
	if len(store.discarded) != 1 || store.discarded[0] != 1 {
		t.Fatalf("unsent code not discarded: %v", store.discarded)
	}
}

func post(engine *gin.Engine, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	return w
}

func TestForgotPasswordDoesNotRevealAccounts(t *testing.T) {
	engine, store := newResetEngine(t)

	known := post(engine, "/api/v1/auth/forgot-password", `{"email":"User@Example.com"}`)
	unknown := post(engine, "/api/v1/auth/forgot-password", `{"email":"nobody@example.com"}`)
	disabled := post(engine, "/api/v1/auth/forgot-password", `{"email":"disabled@example.com"}`)

	for name, w := range map[string]*httptest.ResponseRecorder{"known": known, "unknown": unknown, "disabled": disabled} {
		if w.Code != http.StatusAccepted {
			t.Errorf("%s: status %d, want 202", name, w.Code)
		}
		if w.Body.String() != known.Body.String() {
			t.Errorf("%s: body differs from known account: %s", name, w.Body.String())
		}
	}
	if store.issued != 1 {
		t.Errorf("issued %d codes, want 1 (only the active account)", store.issued)
	}
}

func TestForgotPasswordRejectsMalformedEmail(t *testing.T) {
	engine, store := newResetEngine(t)
	for _, body := range []string{`{}`, `{"email":"not-an-email"}`, `{"email":"a@b.com\r\nBcc: x@y.com"}`} {
		if w := post(engine, "/api/v1/auth/forgot-password", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, w.Code)
		}
	}
	if store.issued != 0 {
		t.Error("code issued for malformed request")
	}
}

func TestVerifyResetCodeValidationAndGenericFailure(t *testing.T) {
	engine, store := newResetEngine(t)

	for name, body := range map[string]string{
		"letters in otp": `{"email":"user@example.com","code":"12345a"}`,
		"short otp":      `{"email":"user@example.com","code":"123"}`,
		"no email":       `{"code":"123456"}`,
	} {
		if w := post(engine, "/api/v1/auth/verify-reset-code", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}

	// Unknown and disabled accounts fail exactly like a wrong code.
	var bodies []string
	for _, email := range []string{"nobody@example.com", "disabled@example.com"} {
		w := post(engine, "/api/v1/auth/verify-reset-code", `{"email":"`+email+`","code":"123456"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", email, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	store.codeOK = false
	wrong := post(engine, "/api/v1/auth/verify-reset-code", `{"email":"user@example.com","code":"123456"}`)
	bodies = append(bodies, wrong.Body.String())
	for _, b := range bodies[1:] {
		if b != bodies[0] || !strings.Contains(b, "invalid_reset_code") {
			t.Errorf("failure responses differ or leak: %v", bodies)
		}
	}
}

func verifyCode(t *testing.T, engine *gin.Engine) string {
	t.Helper()
	w := post(engine, "/api/v1/auth/verify-reset-code", `{"email":"user@example.com","code":"123456"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status %d, body %s", w.Code, w.Body.String())
	}
	var resp handlers.VerifyResetCodeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ResetToken == "" || resp.ExpiresIn <= 0 || resp.ExpiresIn > 600 {
		t.Fatalf("unexpected verify response %+v", resp)
	}
	return resp.ResetToken
}

func TestResetPasswordFlowUsesTokenOnce(t *testing.T) {
	engine, store := newResetEngine(t)
	token := verifyCode(t, engine)

	// The code is spent: verifying it again fails.
	if w := post(engine, "/api/v1/auth/verify-reset-code", `{"email":"user@example.com","code":"123456"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("code reused: status %d", w.Code)
	}

	// A weak password is rejected without spending the token.
	if w := post(engine, "/api/v1/auth/reset-password", `{"reset_token":"`+token+`","new_password":"short"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("short password: status %d", w.Code)
	}

	body := `{"reset_token":"` + token + `","new_password":"a-good-password"}`
	if w := post(engine, "/api/v1/auth/reset-password", body); w.Code != http.StatusOK {
		t.Fatalf("valid reset: status %d, body %s", w.Code, w.Body.String())
	}
	if !store.redeemed {
		t.Fatal("password not reset")
	}
	w := post(engine, "/api/v1/auth/reset-password", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_reset_token") {
		t.Fatalf("token reused: status %d, body %s", w.Code, w.Body.String())
	}
}

func TestResetPasswordRejectsBadTokens(t *testing.T) {
	engine, _ := newResetEngine(t)
	for name, body := range map[string]string{
		"missing token":   `{"new_password":"a-good-password"}`,
		"malformed token": `{"reset_token":"not a token","new_password":"a-good-password"}`,
		"unknown token":   `{"reset_token":"` + strings.Repeat("A", 43) + `","new_password":"a-good-password"}`,
		"old otp shape":   `{"email":"user@example.com","code":"123456","new_password":"a-good-password"}`,
	} {
		if w := post(engine, "/api/v1/auth/reset-password", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}
}
