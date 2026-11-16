package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"notification-job-mb/internal/mailer"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

const (
	MinPasswordLen = 8
	MaxPasswordLen = 128

	resetOTPDigits  = 6
	resetOTPTTL     = 10 * time.Minute
	resetCooldown   = 60 * time.Second
	resetHourlyCap  = 5
	resetMaxAttempt = 5 // wrong guesses before a code dies

	// The reset token issued for a verified code: single-use, short-lived.
	resetTokenTTL   = 10 * time.Minute
	resetTokenBytes = 32

	// maxConcurrentResetHashes bounds Argon2id (64 MiB each) on the reset path.
	maxConcurrentResetHashes = 2

	resetEmailSubject = "Mã xác thực đặt lại mật khẩu"
)

var (
	ErrInvalidResetCode  = errors.New("auth: invalid or expired reset code")
	ErrInvalidResetToken = errors.New("auth: invalid or expired reset token")
	ErrWeakPassword      = errors.New("auth: password length out of range")
)

type resetUserStore interface {
	FindByEmail(ctx context.Context, email string) (*models.User, error)
}

type resetStore interface {
	Issue(ctx context.Context, otp *models.PasswordResetOTP, now time.Time, limits repositories.ResetLimits) error
	Discard(ctx context.Context, otpID uint) error
	Verify(ctx context.Context, userID uint, codeHash string, now time.Time, maxAttempts int, token repositories.ResetToken) error
	Redeem(ctx context.Context, tokenHash string, now time.Time, newHash func() (string, error)) error
}

// mailQueue hands email to background delivery without blocking.
type mailQueue interface {
	Enqueue(msg mailer.Message, expiresAt time.Time) error
}

// PasswordResetService implements the three-step forgot-password flow: an OTP
// is emailed (asynchronously, via the mail queue), the OTP is verified in
// exchange for a short-lived reset token, and the token is exchanged once for a
// new password.
type PasswordResetService struct {
	users     resetUserStore
	store     resetStore
	mail      mailQueue
	passwords *PasswordService
	key       []byte // HMAC key for OTP hashes, derived from the server secret
	now       func() time.Time

	hashSlots chan struct{}
}

func NewPasswordResetService(
	users resetUserStore,
	store resetStore,
	mail mailQueue,
	passwords *PasswordService,
	secret string,
) (*PasswordResetService, error) {
	if len(secret) < minAccessSecretLen {
		return nil, fmt.Errorf("reset: secret must be at least %d bytes", minAccessSecretLen)
	}
	// Domain-separate from the JWT use of the same secret.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("password-reset-otp:v1"))

	return &PasswordResetService{
		users:     users,
		store:     store,
		mail:      mail,
		passwords: passwords,
		key:       mac.Sum(nil),
		now:       time.Now,
		hashSlots: make(chan struct{}, maxConcurrentResetHashes),
	}, nil
}

// RequestReset step 1: queue an OTP email for the account, if there is one.
//
// It returns nil whether or not the email belongs to an active account, and
// when a rate limit suppresses the code, so the response cannot be used to
// enumerate accounts. The email is only queued, never sent inline, so response
// time does not depend on the SMTP server either.
func (s *PasswordResetService) RequestReset(ctx context.Context, email string) error {
	user, err := s.users.FindByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, repositories.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !user.IsActive {
		return nil
	}

	code, err := newOTP()
	if err != nil {
		return err
	}
	now := s.now()

	otp := &models.PasswordResetOTP{
		UserID:    user.ID,
		CodeHash:  s.codeHash(user.ID, code),
		ExpiresAt: now.Add(resetOTPTTL),
		CreatedAt: now,
	}
	err = s.store.Issue(ctx, otp, now, repositories.ResetLimits{Cooldown: resetCooldown, HourlyCap: resetHourlyCap})
	if errors.Is(err, repositories.ErrResetRateLimited) || errors.Is(err, repositories.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	// The code is committed before it is queued, so the email can never carry
	// a code that is not redeemable. The plaintext code exists only in memory.
	err = s.mail.Enqueue(mailer.Message{
		ID:      fmt.Sprintf("password-reset-%d", otp.ID),
		To:      user.Email,
		Subject: resetEmailSubject,
		Body:    resetEmailBody(user.Name, code),
	}, otp.ExpiresAt)
	if err != nil {
		// Overloaded or shutting down. Discard the code so the cooldown does not
		// stop the user from retrying right away. The response stays the same
		// as for unknown accounts; a distinct error would only be reachable for
		// real accounts and so would reveal them.
		log.Printf("password reset: queue email for code %d: %v", otp.ID, err)
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if derr := s.store.Discard(dctx, otp.ID); derr != nil {
			log.Printf("password reset: discard code %d: %v", otp.ID, derr)
		}
	}
	return nil
}

// VerifyResetCode step 2: check the OTP and return a single-use reset token
// valid for resetTokenTTL. The code is consumed on success; wrong guesses count
// against it.
func (s *PasswordResetService) VerifyResetCode(ctx context.Context, email, code string) (string, time.Time, error) {
	if !validOTPFormat(code) {
		return "", time.Time{}, ErrInvalidResetCode
	}

	user, err := s.users.FindByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, repositories.ErrNotFound) {
		return "", time.Time{}, ErrInvalidResetCode
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if !user.IsActive {
		return "", time.Time{}, ErrInvalidResetCode
	}

	raw, err := newResetToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	token := repositories.ResetToken{Hash: hashResetToken(raw), ExpiresAt: now.Add(resetTokenTTL)}

	err = s.store.Verify(ctx, user.ID, s.codeHash(user.ID, code), now, resetMaxAttempt, token)
	if errors.Is(err, repositories.ErrResetCodeInvalid) {
		return "", time.Time{}, ErrInvalidResetCode
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return raw, token.ExpiresAt, nil
}

// ResetPassword step 3: exchange the reset token for a new password. On
// success the token is burned and every session of the user is revoked.
func (s *PasswordResetService) ResetPassword(ctx context.Context, resetToken, newPassword string) error {
	if len(newPassword) < MinPasswordLen || len(newPassword) > MaxPasswordLen {
		return ErrWeakPassword
	}
	if !validResetTokenFormat(resetToken) {
		return ErrInvalidResetToken
	}

	err := s.store.Redeem(ctx, hashResetToken(resetToken), s.now(), func() (string, error) {
		return s.hashPassword(ctx, newPassword)
	})
	if errors.Is(err, repositories.ErrResetTokenInvalid) {
		return ErrInvalidResetToken
	}
	return err
}

func (s *PasswordResetService) hashPassword(ctx context.Context, password string) (string, error) {
	select {
	case s.hashSlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.hashSlots }()
	return s.passwords.Hash(password)
}

// codeHash binds the code to its user so a code cannot be replayed across accounts.
func (s *PasswordResetService) codeHash(userID uint, code string) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%d:%s", userID, code)
	return hex.EncodeToString(mac.Sum(nil))
}

func newOTP() (string, error) {
	bound := new(big.Int).Exp(big.NewInt(10), big.NewInt(resetOTPDigits), nil)
	n, err := rand.Int(rand.Reader, bound)
	if err != nil {
		return "", fmt.Errorf("reset: generate otp: %w", err)
	}
	return fmt.Sprintf("%0*d", resetOTPDigits, n), nil
}

func newResetToken() (string, error) {
	b := make([]byte, resetTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reset: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashResetToken uses SHA-256 for the same reason as refresh tokens: 256 random
// bits need no slow hash, and an unsalted hash allows indexed lookup.
func hashResetToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func validResetTokenFormat(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(resetTokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}

func validOTPFormat(code string) bool {
	if len(code) != resetOTPDigits {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

func resetEmailBody(name, code string) string {
	return fmt.Sprintf(
		"Xin chào %s,\n\n"+
			"Mã xác thực để đặt lại mật khẩu của bạn là: %s\n\n"+
			"Mã có hiệu lực trong %d phút và chỉ dùng được một lần. "+
			"Không chia sẻ mã này với bất kỳ ai.\n\n"+
			"Nếu bạn không yêu cầu đặt lại mật khẩu, hãy bỏ qua email này.\n",
		name, code, int(resetOTPTTL.Minutes()),
	)
}
