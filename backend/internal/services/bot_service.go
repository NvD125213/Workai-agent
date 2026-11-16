package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxBotMessageRunes is the Telegram limit for the text of one message.
const MaxBotMessageRunes = 4096

var (
	// ErrBotMessageInvalid: empty text or longer than MaxBotMessageRunes.
	ErrBotMessageInvalid = errors.New("bot: message must be 1-4096 characters")
	// ErrBotUnavailable: Telegram is down or refused the request (bad token, chat...).
	ErrBotUnavailable = errors.New("bot: telegram unavailable")
)

type messageSender interface {
	SendMessage(ctx context.Context, text string) error
}

// BotService sends notifications to the configured Telegram chat.
type BotService struct {
	sender messageSender
}

func NewBotService(sender messageSender) *BotService {
	return &BotService{sender: sender}
}

func (s *BotService) SendMessage(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > MaxBotMessageRunes {
		return ErrBotMessageInvalid
	}
	if err := s.sender.SendMessage(ctx, text); err != nil {
		return fmt.Errorf("%w: %w", ErrBotUnavailable, err)
	}
	return nil
}
