package services

import (
	"context"
	"errors"
	"testing"

	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

type fakeTelegramUsers struct {
	created []*models.User
	err     error
}

func (f *fakeTelegramUsers) FindByTelegramID(_ context.Context, id int64) (*models.User, error) {
	for _, u := range f.created {
		if *u.TelegramID == id {
			return u, nil
		}
	}
	return nil, repositories.ErrNotFound
}

func (f *fakeTelegramUsers) Create(_ context.Context, u *models.User) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, u)
	return nil
}

func TestTelegramRegister(t *testing.T) {
	s := NewTelegramRegisterService(&fakeTelegramUsers{})

	u, err := s.Register(context.Background(), 42, "  Nguyễn Văn A ", " A@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "a@example.com" || u.Name != "Nguyễn Văn A" || *u.TelegramID != 42 || u.PasswordHash != "" {
		t.Errorf("user = %+v", u)
	}
}

func TestTelegramRegisterErrors(t *testing.T) {
	tests := map[string]struct {
		name, email string
		storeErr    error
		want        error
	}{
		"empty name":     {" ", "a@b.co", nil, ErrInvalidName},
		"bad email":      {"A", "not-an-email", nil, ErrInvalidEmail},
		"display email":  {"A", "A <a@b.co>", nil, ErrInvalidEmail},
		"no tld":         {"A", "a@gmail", nil, ErrInvalidEmail},
		"email taken":    {"A", "a@b.co", repositories.ErrEmailTaken, ErrEmailRegistered},
		"already joined": {"A", "a@b.co", repositories.ErrTelegramIDTaken, ErrTelegramRegistered},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := NewTelegramRegisterService(&fakeTelegramUsers{err: tt.storeErr})
			if _, err := s.Register(context.Background(), 1, tt.name, tt.email); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
