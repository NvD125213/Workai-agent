package services

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode/utf8"

	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

var (
	// ErrInvalidName: the name is empty or longer than the users.name column.
	ErrInvalidName = errors.New("telegram register: invalid name")
	// ErrInvalidEmail: the email is empty, malformed or too long.
	ErrInvalidEmail = errors.New("telegram register: invalid email")
	// ErrTelegramRegistered: this Telegram account already has a user.
	ErrTelegramRegistered = repositories.ErrTelegramIDTaken
	// ErrEmailRegistered: the email belongs to another user.
	ErrEmailRegistered = repositories.ErrEmailTaken
)

type telegramUserStore interface {
	FindByTelegramID(ctx context.Context, telegramID int64) (*models.User, error)
	Create(ctx context.Context, user *models.User) error
}

// TelegramRegisterService creates password-less users for the Telegram
// accounts that sign up by chatting with the bot.
type TelegramRegisterService struct {
	users telegramUserStore
}

func NewTelegramRegisterService(users telegramUserStore) *TelegramRegisterService {
	return &TelegramRegisterService{users: users}
}

// FindByTelegramID returns repositories.ErrNotFound when the account has not registered.
func (s *TelegramRegisterService) FindByTelegramID(ctx context.Context, telegramID int64) (*models.User, error) {
	return s.users.FindByTelegramID(ctx, telegramID)
}

// Register creates the user of a Telegram account. telegramID must come from
// Telegram itself (the sender of a bot update), never from user input.
func (s *TelegramRegisterService) Register(ctx context.Context, telegramID int64, name, email string) (*models.User, error) {
	name, err := NormalizeRegistrationName(name)
	if err != nil {
		return nil, err
	}
	email = NormalizeEmail(email)
	if !validEmail(email) {
		return nil, ErrInvalidEmail
	}

	user := &models.User{
		Email:      email,
		Name:       name,
		Role:       "USER",
		IsActive:   true,
		TelegramID: &telegramID,
	}
	if err := s.users.Create(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// Limits of the users.name and users.email columns.
const (
	maxNameRunes  = 100
	maxEmailBytes = 150
)

// NormalizeRegistrationName trims the name and checks it fits users.name; the
// bot validates the name with it before asking for the email.
func NormalizeRegistrationName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameRunes {
		return "", ErrInvalidName
	}
	return name, nil
}

// validEmail accepts a bare address only ("a@b.c", not "Name <a@b.c>").
func validEmail(email string) bool {
	if email == "" || len(email) > maxEmailBytes {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email && strings.Contains(email[strings.LastIndex(email, "@"):], ".")
}
