package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"notification-job-mb/internal/models"
)

var (
	// ErrNotFound is returned when the requested record does not exist.
	ErrNotFound = errors.New("repository: record not found")
	// ErrEmailTaken: another user already has this email.
	ErrEmailTaken = errors.New("repository: email already registered")
	// ErrTelegramIDTaken: another user is already linked to this Telegram account.
	ErrTelegramIDTaken = errors.New("repository: telegram account already registered")
)

type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

// FindByEmail looks up a user by an already-normalized (trimmed, lower-case) email.
func (r *UserRepository) FindByEmail(ctx context.Context, email string) (*models.User, error) {
	var user models.User
	err := r.db.WithContext(ctx).Where("email = ?", email).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by email: %w", err)
	}
	return &user, nil
}

// Create inserts a new user. The caller must pass an already-normalized email.
// The unique indexes arbitrate concurrent sign-ups: the loser gets
// ErrEmailTaken or ErrTelegramIDTaken.
func (r *UserRepository) Create(ctx context.Context, user *models.User) error {
	err := r.db.WithContext(ctx).Create(user).Error
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "idx_users_email":
			return ErrEmailTaken
		case "idx_users_telegram_id":
			return ErrTelegramIDTaken
		}
	}
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *UserRepository) FindByTelegramID(ctx context.Context, telegramID int64) (*models.User, error) {
	var user models.User
	err := r.db.WithContext(ctx).Where("telegram_id = ?", telegramID).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by telegram id: %w", err)
	}
	return &user, nil
}

func (r *UserRepository) FindByID(ctx context.Context, id uint) (*models.User, error) {
	var user models.User
	err := r.db.WithContext(ctx).Where("id = ?", id).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by id: %w", err)
	}
	return &user, nil
}
