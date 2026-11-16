package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"notification-job-mb/internal/config"
	"notification-job-mb/internal/database"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

// These tests exercise real PostgreSQL semantics (row locks, concurrent
// redeem). They run only when TEST_DB_NAME names a scratch database, and they
// truncate its tables. DB_* settings come from the environment, e.g.
//
//	set -a; . ./.env; set +a; TEST_DB_NAME=nj_test go test ./tests/integration/
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := os.Getenv("TEST_DB_NAME")
	if name == "" {
		t.Skip("TEST_DB_NAME not set")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DB.Name = name
	db, err := database.Connect(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("TRUNCATE users, password_reset_otps, refresh_tokens RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func newUser(t *testing.T, db *gorm.DB) *models.User {
	t.Helper()
	u := &models.User{Email: "a@example.com", Name: "A", PasswordHash: "old", Role: models.RoleUser, IsActive: true}
	if err := db.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

var limits = repositories.ResetLimits{Cooldown: time.Minute, HourlyCap: 3}

func issue(t *testing.T, repo *repositories.PasswordResetRepository, userID uint, codeHash string, now time.Time) error {
	t.Helper()
	otp := &models.PasswordResetOTP{UserID: userID, CodeHash: codeHash, ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
	return repo.Issue(context.Background(), otp, now, limits)
}

// verify checks codeHash and, on a match, attaches a reset token with hash
// tokenHash expiring 10 minutes after now.
func verify(repo *repositories.PasswordResetRepository, userID uint, codeHash, tokenHash string, now time.Time) error {
	token := repositories.ResetToken{Hash: tokenHash, ExpiresAt: now.Add(10 * time.Minute)}
	return repo.Verify(context.Background(), userID, codeHash, now, 5, token)
}

func countCodes(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.PasswordResetOTP{}).Where("user_id = ?", userID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func passwordHash(t *testing.T, db *gorm.DB, userID uint) string {
	t.Helper()
	var u models.User
	if err := db.First(&u, userID).Error; err != nil {
		t.Fatal(err)
	}
	return u.PasswordHash
}

func newHash(h string) func() (string, error) { return func() (string, error) { return h, nil } }

func TestIssueRateLimits(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()

	if err := issue(t, repo, u.ID, "h1", now); err != nil {
		t.Fatal(err)
	}
	if err := issue(t, repo, u.ID, "h2", now.Add(30*time.Second)); !errors.Is(err, repositories.ErrResetRateLimited) {
		t.Fatalf("within cooldown: got %v, want ErrResetRateLimited", err)
	}
	if err := issue(t, repo, u.ID, "h2", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := issue(t, repo, u.ID, "h3", now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := issue(t, repo, u.ID, "h4", now.Add(6*time.Minute)); !errors.Is(err, repositories.ErrResetRateLimited) {
		t.Fatalf("over hourly cap: got %v, want ErrResetRateLimited", err)
	}

	// A new code invalidates the previous one.
	if err := verify(repo, u.ID, "h1", "tok", now.Add(5*time.Minute)); !errors.Is(err, repositories.ErrResetCodeInvalid) {
		t.Fatalf("superseded code accepted: %v", err)
	}
	if n := countCodes(t, db, u.ID); n != 3 {
		t.Fatalf("codes = %d, want 3 (rate-limited requests must not store a code)", n)
	}

	// Once the window has passed, old codes are deleted on the next issue, so
	// the table stays bounded per user without a cleanup job.
	if err := issue(t, repo, u.ID, "h5", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := countCodes(t, db, u.ID); n != 1 {
		t.Fatalf("codes after window = %d, want 1", n)
	}
}

func TestDiscardFreesRateLimit(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()

	otp := &models.PasswordResetOTP{UserID: u.ID, CodeHash: "h1", ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
	if err := repo.Issue(context.Background(), otp, now, limits); err != nil {
		t.Fatal(err)
	}
	if err := repo.Discard(context.Background(), otp.ID); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "h1", "tok", now); !errors.Is(err, repositories.ErrResetCodeInvalid) {
		t.Fatalf("discarded code accepted: %v", err)
	}
	// No cooldown applies: the discarded code was never emailed.
	if err := issue(t, repo, u.ID, "h2", now.Add(time.Second)); err != nil {
		t.Fatalf("retry after discard: %v", err)
	}
}

func TestVerifyLocksAfterMaxAttempts(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if err := verify(repo, u.ID, "wrong", "tok", now); !errors.Is(err, repositories.ErrResetCodeInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// The correct code no longer works: guesses are capped per code.
	if err := verify(repo, u.ID, "right", "tok", now); !errors.Is(err, repositories.ErrResetCodeInvalid) {
		t.Fatalf("correct code accepted after lockout: %v", err)
	}
	if err := repo.Redeem(context.Background(), "tok", now, newHash("x")); !errors.Is(err, repositories.ErrResetTokenInvalid) {
		t.Fatalf("token usable despite lockout: %v", err)
	}
}

func TestVerifyExpiredCode(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "right", "tok", now.Add(11*time.Minute)); !errors.Is(err, repositories.ErrResetCodeInvalid) {
		t.Fatalf("expired code accepted: %v", err)
	}
}

func TestVerifyThenRedeemIsSingleUseAndRevokesSessions(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	db.Create(&models.RefreshToken{UserID: u.ID, TokenHash: "t1", ExpiresAt: now.Add(time.Hour)})
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}

	if err := verify(repo, u.ID, "right", "tok", now); err != nil {
		t.Fatal(err)
	}
	// Verifying alone changes no password, and spends the code.
	if passwordHash(t, db, u.ID) != "old" {
		t.Fatal("password changed by verify")
	}
	if err := verify(repo, u.ID, "right", "tok2", now); !errors.Is(err, repositories.ErrResetCodeInvalid) {
		t.Fatalf("code reused: %v", err)
	}

	if err := repo.Redeem(context.Background(), "tok", now.Add(time.Minute), newHash("new-hash")); err != nil {
		t.Fatal(err)
	}
	if got := passwordHash(t, db, u.ID); got != "new-hash" {
		t.Fatalf("password hash = %q", got)
	}
	var tok models.RefreshToken
	db.Where("token_hash = ?", "t1").First(&tok)
	if tok.RevokedAt == nil {
		t.Fatal("refresh token not revoked")
	}
	if err := repo.Redeem(context.Background(), "tok", now.Add(time.Minute), newHash("again")); !errors.Is(err, repositories.ErrResetTokenInvalid) {
		t.Fatalf("token reused: %v", err)
	}
}

func TestRedeemExpiredToken(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "right", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Redeem(context.Background(), "tok", now.Add(11*time.Minute), newHash("x")); !errors.Is(err, repositories.ErrResetTokenInvalid) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if passwordHash(t, db, u.ID) != "old" {
		t.Fatal("password changed by expired token")
	}
}

func TestNewCodeRevokesOutstandingToken(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "right", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := issue(t, repo, u.ID, "next", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Redeem(context.Background(), "tok", now.Add(3*time.Minute), newHash("x")); !errors.Is(err, repositories.ErrResetTokenInvalid) {
		t.Fatalf("superseded token accepted: %v", err)
	}
}

func TestRedeemRejectsInactiveUser(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "right", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(u).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.Redeem(context.Background(), "tok", now, newHash("x")); !errors.Is(err, repositories.ErrResetTokenInvalid) {
		t.Fatalf("token redeemed for disabled user: %v", err)
	}
}

func TestRedeemConcurrentSingleWinner(t *testing.T) {
	db := testDB(t)
	repo := repositories.NewPasswordResetRepository(db)
	u := newUser(t, db)
	now := time.Now()
	if err := issue(t, repo, u.ID, "right", now); err != nil {
		t.Fatal(err)
	}
	if err := verify(repo, u.ID, "right", "tok", now); err != nil {
		t.Fatal(err)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.Redeem(context.Background(), "tok", now, newHash(fmt.Sprintf("h%d", i)))
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, repositories.ErrResetTokenInvalid) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d redeems succeeded, want exactly 1", wins.Load())
	}
}
