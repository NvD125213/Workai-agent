package repositories

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"notification-job-mb/internal/models"
)

var (
	// ErrResetRateLimited means a reset code was requested too soon or too often.
	ErrResetRateLimited = errors.New("repository: password reset rate limited")
	// ErrResetCodeInvalid covers every verify failure (no live code, wrong code,
	// attempts exhausted) so callers cannot distinguish them.
	ErrResetCodeInvalid = errors.New("repository: password reset code invalid")
	// ErrResetTokenInvalid covers every redeem failure (unknown, expired, used
	// or superseded token, inactive user).
	ErrResetTokenInvalid = errors.New("repository: password reset token invalid")
)

// ResetToken is the hash and expiry of the token issued for a verified code.
type ResetToken struct {
	Hash      string
	ExpiresAt time.Time
}

// ResetLimits bound how often a user can be sent reset codes.
type ResetLimits struct {
	Cooldown  time.Duration // minimum gap between two codes
	HourlyCap int           // maximum codes per rolling hour
}

type PasswordResetRepository struct {
	db *gorm.DB
}

func NewPasswordResetRepository(db *gorm.DB) *PasswordResetRepository {
	return &PasswordResetRepository{db: db}
}

// Issue stores a new OTP, invalidating any earlier live code and any
// outstanding reset token. Locking the user
// row serializes concurrent requests so the rate limits cannot be raced past.
// Returns ErrResetRateLimited when a limit applies, and ErrNotFound when the
// user no longer exists.
//
// Codes older than the rate-limit window are deleted here, so the table holds
// at most HourlyCap rows per user and needs no separate cleanup job.
func (r *PasswordResetRepository) Issue(
	ctx context.Context,
	otp *models.PasswordResetOTP,
	now time.Time,
	limits ResetLimits,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockUser(tx, otp.UserID); err != nil {
			return err
		}

		if err := tx.Where("user_id = ? AND created_at <= ?", otp.UserID, now.Add(-time.Hour)).
			Delete(&models.PasswordResetOTP{}).Error; err != nil {
			return fmt.Errorf("delete old reset codes: %w", err)
		}

		// Find (not Take): "no previous code" is the normal case, not an error to log.
		var last []models.PasswordResetOTP
		if err := tx.Where("user_id = ?", otp.UserID).Order("created_at DESC").Limit(1).Find(&last).Error; err != nil {
			return fmt.Errorf("find last reset code: %w", err)
		}
		if len(last) == 1 && now.Sub(last[0].CreatedAt) < limits.Cooldown {
			return ErrResetRateLimited
		}

		var recent int64
		if err := tx.Model(&models.PasswordResetOTP{}).
			Where("user_id = ? AND created_at > ?", otp.UserID, now.Add(-time.Hour)).
			Count(&recent).Error; err != nil {
			return fmt.Errorf("count reset codes: %w", err)
		}
		if recent >= int64(limits.HourlyCap) {
			return ErrResetRateLimited
		}

		if err := tx.Model(&models.PasswordResetOTP{}).
			Where("user_id = ? AND consumed_at IS NULL", otp.UserID).
			Update("consumed_at", now).Error; err != nil {
			return fmt.Errorf("invalidate previous reset codes: %w", err)
		}
		if err := tx.Model(&models.PasswordResetOTP{}).
			Where("user_id = ? AND reset_token_hash IS NOT NULL", otp.UserID).
			Update("reset_token_hash", nil).Error; err != nil {
			return fmt.Errorf("invalidate previous reset tokens: %w", err)
		}
		if err := tx.Create(otp).Error; err != nil {
			return fmt.Errorf("create reset code: %w", err)
		}
		return nil
	})
}

// Discard deletes a code whose email could not be queued, so it neither stays
// redeemable nor counts against the user's rate limits.
func (r *PasswordResetRepository) Discard(ctx context.Context, otpID uint) error {
	if err := r.db.WithContext(ctx).Delete(&models.PasswordResetOTP{}, otpID).Error; err != nil {
		return fmt.Errorf("discard reset code: %w", err)
	}
	return nil
}

// Verify checks codeHash against the user's live OTP and, on a match,
// consumes the code and attaches token to it. Each attempt is counted and
// committed even when it fails, so a code dies after maxAttempts wrong guesses.
func (r *PasswordResetRepository) Verify(
	ctx context.Context,
	userID uint,
	codeHash string,
	now time.Time,
	maxAttempts int,
	token ResetToken,
) error {
	var outcome error
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockUser(tx, userID); err != nil {
			if errors.Is(err, ErrNotFound) {
				outcome = ErrResetCodeInvalid
				return nil
			}
			return err
		}

		var live []models.PasswordResetOTP
		err := tx.Where("user_id = ? AND consumed_at IS NULL AND expires_at > ? AND attempts < ?", userID, now, maxAttempts).
			Order("created_at DESC").
			Limit(1).
			Find(&live).Error
		if err != nil {
			return fmt.Errorf("find reset code: %w", err)
		}
		if len(live) == 0 {
			outcome = ErrResetCodeInvalid
			return nil
		}
		otp := live[0]

		otp.Attempts++
		if err := tx.Model(&otp).Update("attempts", otp.Attempts).Error; err != nil {
			return fmt.Errorf("count reset attempt: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(otp.CodeHash), []byte(codeHash)) != 1 {
			outcome = ErrResetCodeInvalid
			return nil // commit the attempt counter
		}

		if err := tx.Model(&otp).Updates(map[string]any{
			"consumed_at":            now,
			"reset_token_hash":       token.Hash,
			"reset_token_expires_at": token.ExpiresAt,
		}).Error; err != nil {
			return fmt.Errorf("consume reset code: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return outcome
}

// Redeem exchanges a live reset token for a new password: it sets the new
// password hash, burns the token and revokes every refresh token of the user —
// all atomically. The token works once.
//
// newHash runs only after the token matched, so invalid tokens never trigger
// an expensive password hash.
func (r *PasswordResetRepository) Redeem(
	ctx context.Context,
	tokenHash string,
	now time.Time,
	newHash func() (string, error),
) error {
	var outcome error
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The row lock makes concurrent redeems of one token single-winner.
		var live []models.PasswordResetOTP
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("reset_token_hash = ? AND reset_token_expires_at > ?", tokenHash, now).
			Limit(1).
			Find(&live).Error
		if err != nil {
			return fmt.Errorf("find reset token: %w", err)
		}
		if len(live) == 0 {
			outcome = ErrResetTokenInvalid
			return nil
		}
		otp := live[0]

		var active []models.User
		if err := tx.Select("id").Where("id = ? AND is_active", otp.UserID).Limit(1).Find(&active).Error; err != nil {
			return fmt.Errorf("check user: %w", err)
		}
		if len(active) == 0 {
			outcome = ErrResetTokenInvalid
			return nil
		}

		hash, err := newHash()
		if err != nil {
			return err
		}
		if err := tx.Model(&otp).Update("reset_token_hash", nil).Error; err != nil {
			return fmt.Errorf("burn reset token: %w", err)
		}
		if err := tx.Model(&models.User{}).Where("id = ?", otp.UserID).Update("password_hash", hash).Error; err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if err := tx.Model(&models.RefreshToken{}).
			Where("user_id = ? AND revoked_at IS NULL", otp.UserID).
			Update("revoked_at", now).Error; err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return outcome
}

// lockUser takes a row lock on the user for the rest of the transaction.
func lockUser(tx *gorm.DB, userID uint) error {
	var u models.User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", userID).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	return nil
}
