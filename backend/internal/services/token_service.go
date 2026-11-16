package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	minAccessSecretLen = 32
	refreshTokenBytes  = 32
)

var ErrInvalidAccessToken = errors.New("token: invalid access token")

type TokenConfig struct {
	AccessSecret string
	Issuer       string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
}

// AccessClaims is the verified identity carried by an access token.
type AccessClaims struct {
	UserID uint
	Role   string
}

type accessTokenClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type TokenService struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	parser     *jwt.Parser
}

func NewTokenService(cfg TokenConfig) (*TokenService, error) {
	if len(cfg.AccessSecret) < minAccessSecretLen {
		return nil, fmt.Errorf("token: JWT_ACCESS_SECRET must be at least %d bytes", minAccessSecretLen)
	}
	if cfg.Issuer == "" {
		return nil, errors.New("token: issuer is required")
	}
	if cfg.AccessTTL <= 0 || cfg.RefreshTTL <= 0 {
		return nil, errors.New("token: access and refresh TTL must be positive")
	}

	return &TokenService{
		secret:     []byte(cfg.AccessSecret),
		issuer:     cfg.Issuer,
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
		),
	}, nil
}

func (s *TokenService) AccessTTL() time.Duration  { return s.accessTTL }
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccessToken signs a short-lived HS256 JWT for the user.
func (s *TokenService) IssueAccessToken(userID uint, role string, now time.Time) (string, error) {
	claims := accessTokenClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   strconv.FormatUint(uint64(userID), 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("token: sign access token: %w", err)
	}
	return signed, nil
}

// ParseAccessToken verifies signature, algorithm, issuer and expiry.
func (s *TokenService) ParseAccessToken(raw string) (*AccessClaims, error) {
	var claims accessTokenClaims
	_, err := s.parser.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return s.secret, nil
	})
	if err != nil {
		return nil, ErrInvalidAccessToken
	}

	id, err := strconv.ParseUint(claims.Subject, 10, 0)
	if err != nil || id == 0 {
		return nil, ErrInvalidAccessToken
	}
	return &AccessClaims{UserID: uint(id), Role: claims.Role}, nil
}

// NewRefreshToken returns an opaque random token for the client and the hash
// to persist. Only the hash is ever stored.
func (s *TokenService) NewRefreshToken() (raw, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("token: generate refresh token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, s.HashRefreshToken(raw), nil
}

// HashRefreshToken uses SHA-256: the token is 256 bits of randomness, so a
// slow password hash adds nothing and an unsalted hash allows indexed lookup.
func (s *TokenService) HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
