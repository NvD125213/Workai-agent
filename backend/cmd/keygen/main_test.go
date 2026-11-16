package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"notification-job-mb/internal/services"
)

func TestSecret(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"secret"}, nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.String()))
	if err != nil || len(raw) != secretBytes {
		t.Fatalf("secret %q: decoded %d bytes, err %v", out.String(), len(raw), err)
	}
}

func TestHashPasswordVerifies(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"hash-password"}, strings.NewReader("s3cret pass\r\n"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(out.String())
	if err := services.NewPasswordService().Verify("s3cret pass", hash); err != nil {
		t.Fatalf("hash %q does not verify: %v", hash, err)
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if err := run([]string{"hash-password"}, strings.NewReader("\n"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for empty password")
	}
}
