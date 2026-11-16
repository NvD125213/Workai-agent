package mailer

import (
	"context"
	"strings"
	"testing"
)

func newTestSMTP(t *testing.T) *SMTP {
	t.Helper()
	m, err := NewSMTP(Config{Host: "smtp.example.com", Port: "587", From: "Notify <no-reply@example.com>"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	m := newTestSMTP(t)
	cases := []Message{
		{To: "victim@example.com\r\nBcc: evil@example.com", Subject: "s"},
		{To: "victim@example.com", Subject: "hi\r\nBcc: evil@example.com"},
		{To: "victim@example.com", Subject: "s", ID: "x>\r\nBcc: evil@example.com"},
		{To: "a@example.com, b@example.com", Subject: "s"},
	}
	for _, msg := range cases {
		// Validation fails before any network I/O, so this never dials.
		if err := m.Send(context.Background(), msg); err == nil {
			t.Errorf("Send(%+v) succeeded, want rejection", msg)
		}
	}
}

func TestBuildEncodesNonASCII(t *testing.T) {
	m := newTestSMTP(t)
	to, _ := parseAddress("user@example.com")
	raw, err := m.build(Message{ID: "outbox-1", To: "user@example.com", Subject: "Mã xác thực", Body: "Xin chào"}, to)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Subject: =?UTF-8?q?") {
		t.Errorf("subject not RFC 2047 encoded:\n%s", s)
	}
	if !strings.Contains(s, "Message-ID: <outbox-1@example.com>") {
		t.Errorf("missing stable Message-ID:\n%s", s)
	}
	if strings.Contains(strings.SplitN(s, "\r\n\r\n", 2)[0], "ã") {
		t.Error("raw non-ASCII leaked into headers")
	}
}
