package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"notification-job-mb/internal/models"
)

// ErrTokenAlreadyRevoked is returned by Consume when the token exists but was
// already revoked — a sign the token was replayed after rotation.
var ErrTokenAlreadyRevoked = errors.New("repository: refresh token already revoked")

type RefreshTokenRepository struct {
	db *gorm.DB
}

func NewRefreshTokenRepository(db *gorm.DB) *RefreshTokenRepository {
	return &RefreshTokenRepository{db: db}
}

func (r *RefreshTokenRepository) Create(ctx context.Context, token *models.RefreshToken) error {
	if err := r.db.WithContext(ctx).Create(token).Error; err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

// Consume atomically revokes an active (unrevoked, unexpired) token and returns
// it. Concurrent callers presenting the same token cannot both succeed.
//
// Returns ErrTokenAlreadyRevoked (with the token's owner populated) when the
// token was revoked earlier, and ErrNotFound when it is unknown or expired.
func (r *RefreshTokenRepository) Consume(ctx context.Context, tokenHash string, now time.Time) (*models.RefreshToken, error) {
	var token models.RefreshToken
	res := r.db.WithContext(ctx).
		Model(&token).
		Clauses(clause.Returning{}).
		Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now).
		Update("revoked_at", now)
	if res.Error != nil {
		return nil, fmt.Errorf("consume refresh token: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return &token, nil
	}

	var existing models.RefreshToken
	err := r.db.WithContext(ctx).Where("token_hash = ?", tokenHash).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find refresh token: %w", err)
	}
	if existing.RevokedAt != nil {
		return &existing, ErrTokenAlreadyRevoked
	}
	return nil, ErrNotFound // expired
}

// Revoke marks a single token revoked. Revoking an unknown or already revoked
// token is not an error.
func (r *RefreshTokenRepository) Revoke(ctx context.Context, tokenHash string, now time.Time) error {
	err := r.db.WithContext(ctx).
		Model(&models.RefreshToken{}).
		Where("token_hash = ? AND revoked_at IS NULL", tokenHash).
		Update("revoked_at", now).Error
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllForUser revokes every active token of a user.
func (r *RefreshTokenRepository) RevokeAllForUser(ctx context.Context, userID uint, now time.Time) error {
	err := r.db.WithContext(ctx).
		Model(&models.RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now).Error
	if err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}
