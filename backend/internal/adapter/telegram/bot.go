package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Update is one incoming update of getUpdates; only messages are requested.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int64       `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Text      string      `json:"text"`
	Document  *Document   `json:"document"`
	Photo     []PhotoSize `json:"photo"`
}

// Document is a file sent as a file (not as a photo).
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
}

// MaxDownloadBytes is the Bot API limit for files a bot can download.
const MaxDownloadBytes = 20 << 20

// ErrFileTooLarge: the file is larger than the limit given to DownloadFile.
var ErrFileTooLarge = errors.New("telegram: file too large")

// ReplyMarkup is the reply_markup of a sent message: one of
// *ReplyKeyboardMarkup or *ReplyKeyboardRemove.
type ReplyMarkup interface {
	replyMarkup()
}

// ReplyKeyboardMarkup replaces the user's keyboard with buttons; pressing one
// sends its text as a message.
type ReplyKeyboardMarkup struct {
	Keyboard              [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard        bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard       bool               `json:"one_time_keyboard,omitempty"`
	InputFieldPlaceholder string             `json:"input_field_placeholder,omitempty"`
}

type KeyboardButton struct {
	Text string `json:"text"`
}

// ReplyKeyboardRemove hides a keyboard sent with ReplyKeyboardMarkup.
type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

func (*ReplyKeyboardMarkup) replyMarkup() {}
func (*ReplyKeyboardRemove) replyMarkup() {}

// RemoveKeyboard is the markup that hides the current reply keyboard.
var RemoveKeyboard = &ReplyKeyboardRemove{RemoveKeyboard: true}

// GetUpdates long-polls for new messages. timeoutSec must stay below the
// timeout of the client's *http.Client, otherwise every empty poll fails.
// It fails with a 409 Error while a webhook is set for the bot.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	req := struct {
		Offset         int64    `json:"offset"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{Offset: offset, Timeout: timeoutSec, AllowedUpdates: []string{"message"}}
	var out []Update
	if err := c.call(ctx, "getUpdates", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessageTo sends plain text to any chat; markup may be nil.
func (c *Client) SendMessageTo(ctx context.Context, chatID int64, text string, markup ReplyMarkup) error {
	req := struct {
		ChatID      string      `json:"chat_id"`
		Text        string      `json:"text"`
		ReplyMarkup ReplyMarkup `json:"reply_markup,omitempty"`
	}{ChatID: strconv.FormatInt(chatID, 10), Text: truncate(text, maxMessageRunes), ReplyMarkup: markup}
	return c.call(ctx, "sendMessage", req, nil)
}

// SendHTMLTo sends text formatted with Telegram's HTML subset (<b>, <i>, <a href>...),
// without link previews. It is not truncated, since cutting could break a tag:
// the caller keeps it under 4096 characters and escapes the values it inserts.
func (c *Client) SendHTMLTo(ctx context.Context, chatID int64, html string) error {
	req := struct {
		ChatID             string `json:"chat_id"`
		Text               string `json:"text"`
		ParseMode          string `json:"parse_mode"`
		LinkPreviewOptions struct {
			IsDisabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: strconv.FormatInt(chatID, 10), Text: html, ParseMode: "HTML"}
	req.LinkPreviewOptions.IsDisabled = true
	return c.call(ctx, "sendMessage", req, nil)
}

// DownloadFile downloads a file sent to the bot, failing with ErrFileTooLarge
// past maxBytes (at most MaxDownloadBytes).
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if f.FileSize > maxBytes {
		return nil, ErrFileTooLarge
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/file/bot"+c.token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram download: %s", c.redact(err))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram download: %s", c.redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("telegram download: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}
