// Command keygen generates secrets and password hashes for local setup.
//
//	go run ./cmd/keygen secret          # random value for JWT_ACCESS_SECRET
//	go run ./cmd/keygen hash-password   # Argon2id hash for users.password_hash
//	go run ./cmd/keygen generate-user -email a@b.com -name "A B" [-role ADMIN]
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"notification-job-mb/internal/config"
	"notification-job-mb/internal/database"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

const secretBytes = 48 // encodes to 64 base64 chars, above the 32-byte minimum

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: keygen <secret|hash-password|generate-user>")
	}

	switch args[0] {
	case "secret":
		secret, err := GenerateSecret(secretBytes)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, secret)
		return nil

	case "hash-password":
		// The password is read from stdin rather than an argument so it does
		// not end up in shell history or the process list.
		fmt.Fprint(stderr, "Password: ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read password: %w", err)
		}
		password := strings.TrimRight(line, "\r\n")
		if password == "" {
			return errors.New("password must not be empty")
		}

		hash, err := services.NewPasswordService().Hash(password)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, hash)
		return nil

	case "generate-user":
		return generateUser(args[1:], stdin, stdout, stderr)

	default:
		return fmt.Errorf("unknown command %q (want secret, hash-password or generate-user)", args[0])
	}
}

// generateUser inserts a new user into the database configured via .env / env vars.
//
//	go run ./cmd/keygen generate-user -email a@b.com -name "A B" [-role ADMIN]
//
// The password is read from stdin; if left empty, a random one is generated
// and printed to stderr.
func generateUser(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("generate-user", flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "user email (required)")
	name := fs.String("name", "", "display name (required)")
	role := fs.String("role", models.RoleUser, "USER or ADMIN")
	if err := fs.Parse(args); err != nil {
		return err
	}

	*email = strings.ToLower(strings.TrimSpace(*email))
	*name = strings.TrimSpace(*name)
	*role = strings.ToUpper(strings.TrimSpace(*role))

	if *email == "" || *name == "" {
		return errors.New("-email and -name are required")
	}
	if *role != models.RoleUser && *role != models.RoleAdmin {
		return fmt.Errorf("invalid role %q (want %s or %s)", *role, models.RoleUser, models.RoleAdmin)
	}

	fmt.Fprint(stderr, "Password (empty = generate): ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	generated := password == ""
	if generated {
		if password, err = GenerateSecret(18); err != nil {
			return err
		}
	}

	hash, err := services.NewPasswordService().Hash(password)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	db, err := database.Connect(cfg.DB)
	if err != nil {
		return err
	}
	defer database.Close(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo := repositories.NewUserRepository(db)
	if _, err := repo.FindByEmail(ctx, *email); err == nil {
		return fmt.Errorf("email %q already exists", *email)
	} else if !errors.Is(err, repositories.ErrNotFound) {
		return err
	}

	user := &models.User{
		Email:         *email,
		Name:          *name,
		PasswordHash:  hash,
		Role:          *role,
		IsActive:      true,
		EmailVerified: true,
	}
	if err := repo.Create(ctx, user); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "created user id=%d email=%s role=%s\n", user.ID, user.Email, user.Role)
	if generated {
		fmt.Fprintf(stderr, "generated password: %s\n", password)
	}
	return nil
}

// GenerateSecret returns n cryptographically random bytes, base64-encoded.
func GenerateSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
