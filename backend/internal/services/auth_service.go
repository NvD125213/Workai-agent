package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

// maxConcurrentPasswordChecks bounds Argon2id work: each check allocates
// argon2Memory (64 MiB), so unbounded concurrent logins could exhaust memory.
const maxConcurrentPasswordChecks = 4

var (
	ErrInvalidCredentials  = errors.New("auth: invalid email or password")
	ErrAccountDisabled     = errors.New("auth: account disabled")
	ErrInvalidRefreshToken = errors.New("auth: invalid refresh token")
)

type userStore interface {
	FindByEmail(ctx context.Context, email string) (*models.User, error)
	FindByID(ctx context.Context, id uint) (*models.User, error)
}

type refreshTokenStore interface {
	Create(ctx context.Context, token *models.RefreshToken) error
	Consume(ctx context.Context, tokenHash string, now time.Time) (*models.RefreshToken, error)
	Revoke(ctx context.Context, tokenHash string, now time.Time) error
	RevokeAllForUser(ctx context.Context, userID uint, now time.Time) error
}

// ClientInfo describes the device a session was created from.
type ClientInfo struct {
	UserAgent string
	IPAddress string
}

type AuthResult struct {
	User         *models.User
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration
}

type AuthService struct {
	users     userStore
	tokens    refreshTokenStore
	passwords *PasswordService
	issuer    *TokenService
	now       func() time.Time

	passwordSlots chan struct{}
	// dummyHash is verified when the email is unknown so that response time
	// does not reveal whether an account exists.
	dummyHash string
}

func NewAuthService(
	users userStore,
	tokens refreshTokenStore,
	passwords *PasswordService,
	issuer *TokenService,
) (*AuthService, error) {
	dummy, err := passwords.Hash("dummy-password-for-timing-equalization")
	if err != nil {
		return nil, fmt.Errorf("auth: build dummy hash: %w", err)
	}
	return &AuthService{
		users:         users,
		tokens:        tokens,
		passwords:     passwords,
		issuer:        issuer,
		now:           time.Now,
		passwordSlots: make(chan struct{}, maxConcurrentPasswordChecks),
		dummyHash:     dummy,
	}, nil
}

// NormalizeEmail is the canonical form emails must be stored and looked up in.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s *AuthService) Login(ctx context.Context, email, password string, client ClientInfo) (*AuthResult, error) {
	user, err := s.users.FindByEmail(ctx, NormalizeEmail(email))
	if err != nil && !errors.Is(err, repositories.ErrNotFound) {
		return nil, err
	}

	hash := s.dummyHash
	if user != nil {
		hash = user.PasswordHash
	}
	matched, err := s.passwordMatches(ctx, password, hash)
	if err != nil {
		return nil, err
	}
	if user == nil || !matched {
		return nil, ErrInvalidCredentials
	}
	if !user.IsActive {
		return nil, ErrAccountDisabled
	}

	return s.issueSession(ctx, user, client)
}

// Refresh rotates a refresh token: the presented token is revoked and a new
// pair is issued. Replaying an already rotated token revokes every session of
// its owner, since that indicates the token was stolen.
func (s *AuthService) Refresh(ctx context.Context, rawRefreshToken string, client ClientInfo) (*AuthResult, error) {
	now := s.now()
	old, err := s.tokens.Consume(ctx, s.issuer.HashRefreshToken(rawRefreshToken), now)
	switch {
	case errors.Is(err, repositories.ErrTokenAlreadyRevoked):
		if rerr := s.tokens.RevokeAllForUser(ctx, old.UserID, now); rerr != nil {
			return nil, rerr
		}
		return nil, ErrInvalidRefreshToken
	case errors.Is(err, repositories.ErrNotFound):
		return nil, ErrInvalidRefreshToken
	case err != nil:
		return nil, err
	}

	user, err := s.users.FindByID(ctx, old.UserID)
	if errors.Is(err, repositories.ErrNotFound) {
		return nil, ErrInvalidRefreshToken
	}
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		return nil, ErrAccountDisabled
	}

	return s.issueSession(ctx, user, client)
}

// Logout revokes the given refresh token. It is idempotent.
func (s *AuthService) Logout(ctx context.Context, rawRefreshToken string) error {
	return s.tokens.Revoke(ctx, s.issuer.HashRefreshToken(rawRefreshToken), s.now())
}

// CurrentUser returns the active user behind a verified access token.
func (s *AuthService) CurrentUser(ctx context.Context, userID uint) (*models.User, error) {
	user, err := s.users.FindByID(ctx, userID)
	if errors.Is(err, repositories.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		return nil, ErrAccountDisabled
	}
	return user, nil
}

func (s *AuthService) issueSession(ctx context.Context, user *models.User, client ClientInfo) (*AuthResult, error) {
	now := s.now()

	access, err := s.issuer.IssueAccessToken(user.ID, user.Role, now)
	if err != nil {
		return nil, err
	}

	raw, hash, err := s.issuer.NewRefreshToken()
	if err != nil {
		return nil, err
	}
	if err := s.tokens.Create(ctx, &models.RefreshToken{
		UserID:    user.ID,
		TokenHash: hash,
		ExpiresAt: now.Add(s.issuer.RefreshTTL()),
		UserAgent: client.UserAgent,
		IPAddress: client.IPAddress,
	}); err != nil {
		return nil, err
	}

	return &AuthResult{
		User:         user,
		AccessToken:  access,
		RefreshToken: raw,
		ExpiresIn:    s.issuer.AccessTTL(),
	}, nil
}

// passwordMatches runs the Argon2id check under a concurrency limit. It
// returns an error only for a cancelled wait or a corrupt stored hash.
func (s *AuthService) passwordMatches(ctx context.Context, password, hash string) (bool, error) {
	select {
	case s.passwordSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-s.passwordSlots }()

	err := s.passwords.Verify(password, hash)
	if errors.Is(err, ErrPasswordMismatch) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: verify stored password hash: %w", err)
	}
	return true, nil
}
