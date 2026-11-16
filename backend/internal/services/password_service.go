package services

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Time    = 3
	argon2Memory  = 64 * 1024 // 64 MiB
	argon2Threads = 2
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

var (
	ErrInvalidHash         = errors.New("password: invalid hash format")
	ErrIncompatibleVersion = errors.New("password: incompatible argon2 version")
	ErrPasswordMismatch    = errors.New("password: mismatch")
)

type PasswordService struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
	saltLen uint32
}

func NewPasswordService() *PasswordService {
	return &PasswordService{
		time:    argon2Time,
		memory:  argon2Memory,
		threads: argon2Threads,
		keyLen:  argon2KeyLen,
		saltLen: argon2SaltLen,
	}
}

// Hash mã hoá password bằng Argon2id, trả về chuỗi PHC:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func (s *PasswordService) Hash(password string) (string, error) {
	if password == "" {
		return "", errors.New("password: empty password")
	}

	salt := make([]byte, s.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, s.time, s.memory, s.threads, s.keyLen)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, s.memory, s.time, s.threads, b64Salt, b64Hash,
	)
	return encoded, nil
}

// Verify so khớp password với hash đã lưu (constant-time).
func (s *PasswordService) Verify(password, encodedHash string) error {
	if password == "" || encodedHash == "" {
		return ErrPasswordMismatch
	}

	params, salt, hash, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}

	otherHash := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, params.keyLen)

	if subtle.ConstantTimeCompare(hash, otherHash) == 1 {
		return nil
	}
	return ErrPasswordMismatch
}

type argon2Params struct {
	memory  uint32
	time    uint32
	threads uint8
	keyLen  uint32
}

func decodeHash(encodedHash string) (*argon2Params, []byte, []byte, error) {
	// $argon2id$v=19$m=65536,t=3,p=2$salt$hash
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return nil, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return nil, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return nil, nil, nil, ErrIncompatibleVersion
	}

	params := &argon2Params{}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return nil, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, nil, ErrInvalidHash
	}

	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, nil, ErrInvalidHash
	}
	params.keyLen = uint32(len(hash))

	return params, salt, hash, nil
}
