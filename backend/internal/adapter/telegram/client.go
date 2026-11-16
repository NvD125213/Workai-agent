// Package telegram sends messages through the Telegram Bot API.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	// maxMessageRunes is the Bot API limit for the text of one message.
	maxMessageRunes = 4096
	// maxResponseBytes bounds what is read from the Bot API.
	maxResponseBytes = 1 << 20
)

// Error is a Bot API answer with ok=false. RetryAfter is set on 429.
type Error struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

type Config struct {
	BotToken string
	ChatID   string
	// BaseURL overrides the Bot API server (tests, local Bot API server).
	BaseURL string
	// Timeout bounds one request when no *http.Client is given.
	Timeout time.Duration
}

// Client talks to one bot and sends to one default chat.
type Client struct {
	token   string
	chatID  string
	baseURL string
	hc      *http.Client
}

// NewClient: hc nil uses a client with cfg.Timeout (default 10s).
func NewClient(cfg Config, hc *http.Client) (*Client, error) {
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, errors.New("telegram: TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if hc == nil {
		if cfg.Timeout <= 0 {
			cfg.Timeout = 10 * time.Second
		}
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		token:   cfg.BotToken,
		chatID:  cfg.ChatID,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		hc:      hc,
	}, nil
}

// User is the bot account returned by getMe.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Chat is the default chat returned by getChat.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

// ResourceKind chọn method Bot API và tên tham số chứa file.
type ResourceKind string

const (
	ResourcePhoto    ResourceKind = "photo"
	ResourceVideo    ResourceKind = "video"
	ResourceDocument ResourceKind = "document"
	ResourceAudio    ResourceKind = "audio"
)

// maxCaptionRunes is the Bot API limit for a media caption.
const maxCaptionRunes = 1024

var resourceMethods = map[ResourceKind]string{
	ResourcePhoto:    "sendPhoto",
	ResourceVideo:    "sendVideo",
	ResourceDocument: "sendDocument",
	ResourceAudio:    "sendAudio",
}


// GetMe checks the bot token.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var out User
	if err := c.call(ctx, "getMe", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetChat checks that the bot can reach the configured chat.
func (c *Client) GetChat(ctx context.Context) (*Chat, error) {
	var out Chat
	if err := c.call(ctx, "getChat", map[string]string{"chat_id": c.chatID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ping verifies the token and the chat; use it at startup.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.GetMe(ctx); err != nil {
		return err
	}
	_, err := c.GetChat(ctx)
	return err
}

// SendMessage sends plain text to the configured chat, truncated to the
// Bot API limit.
func (c *Client) SendMessage(ctx context.Context, text string) error {
	req := struct {
		ChatID             string `json:"chat_id"`
		Text               string `json:"text"`
		LinkPreviewOptions struct {
			IsDisabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: c.chatID, Text: truncate(text, maxMessageRunes)}
	req.LinkPreviewOptions.IsDisabled = true
	return c.call(ctx, "sendMessage", req, nil)
}

func (c *Client) SendResourceMessage(ctx context.Context, kind ResourceKind, resourceURL, caption string) error {
	method, ok := resourceMethods[kind]
	if !ok {
		return fmt.Errorf("telegram: unknown resource kind %q", kind)
	}

	if resourceURL == "" {

	}

	req := map[string]any{
		"chat_id": c.chatID,
		"resource_url": resourceURL,
	}
	if caption != "" {
		req["caption"] = truncate(caption, maxCaptionRunes)
	}
	return c.call(ctx, method, req, nil)
}

func (c *Client) call(ctx context.Context, method string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("telegram %s: %s", method, c.redact(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		// *url.Error carries the request URL, which contains the token.
		return fmt.Errorf("telegram %s: %s", method, c.redact(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("telegram %s: read response: %w", method, err)
	}

	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("telegram %s: %d decode response: %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &Error{
			Code:        code,
			Description: r.Description,
			RetryAfter:  time.Duration(r.Parameters.RetryAfter) * time.Second,
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

func (c *Client) redact(err error) string {
	return strings.ReplaceAll(err.Error(), c.token, "<token>")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
