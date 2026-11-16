package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{BotToken: "123:secret", ChatID: "-100", BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendMessage(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:secret/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})

	// Log ra chuỗi kết nối
	log.Printf("Sending message to Telegram bot with token: %s and chat ID: %s", c.token, c.chatID)
	log.Printf("Base URL: %s", c.baseURL)

	if err := c.SendMessage(context.Background(), strings.Repeat("a", 5000)); err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "-100" {
		t.Errorf("chat_id = %v", got["chat_id"])
	}
	if n := len([]rune(got["text"].(string))); n != maxMessageRunes {
		t.Errorf("text runes = %d, want %d", n, maxMessageRunes)
	}

	log.Printf("Client: %+v", c)
}

func TestAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":5}}`))
	})

	err := c.SendMessage(context.Background(), "hi")
	var e *Error
	if !errors.As(err, &e) || e.Code != 429 || e.RetryAfter != 5*time.Second {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportErrorHidesToken(t *testing.T) {
	c, err := NewClient(Config{BotToken: "123:secret", ChatID: "1", BaseURL: "http://127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = c.SendMessage(context.Background(), "hi")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}
