package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"notification-job-mb/internal/adapter/telegram"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

const (
	// pollTimeoutSec: how long one getUpdates waits for a message (long polling).
	pollTimeoutSec = 25
	// signupTTL: a sign-up left unanswered this long starts over at /start.
	signupTTL = 30 * time.Minute
)

type telegramUpdates interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error)
	SendMessageTo(ctx context.Context, chatID int64, text string, markup telegram.ReplyMarkup) error
	SendHTMLTo(ctx context.Context, chatID int64, html string) error
	DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error)
}

type telegramRegistrar interface {
	FindByTelegramID(ctx context.Context, telegramID int64) (*models.User, error)
	Register(ctx context.Context, telegramID int64, name, email string) (*models.User, error)
}

type signupStep int

const (
	stepName signupStep = iota
	stepEmail
)

// signup is the progress of one account through the sign-up conversation.
type signup struct {
	step    signupStep
	name    string
	expires time.Time
}

// TelegramBot talks to users in private chats: /start signs them up (name,
// then email), then a CV sent as a file is analyzed and answered with the
// best matching jobs (see telegram_cv.go).
type TelegramBot struct {
	bot      telegramUpdates
	register telegramRegistrar
	cv       cvAnalyzer
	finder   jobFinder
	cfg      TelegramCVConfig
	now      func() time.Time

	// signups is only touched by the Run goroutine, which handles updates one by one.
	signups map[int64]*signup

	// A CV is processed in its own goroutine; busy holds the Telegram ids being processed.
	mu      sync.Mutex
	busy    map[int64]bool
	running sync.WaitGroup
}

func NewTelegramBot(bot telegramUpdates, register telegramRegistrar, cv cvAnalyzer, finder jobFinder,
	cfg TelegramCVConfig) *TelegramBot {
	cfg.applyDefaults()
	return &TelegramBot{
		bot: bot, register: register, cv: cv, finder: finder, cfg: cfg, now: time.Now,
		signups: map[int64]*signup{}, busy: map[int64]bool{},
	}
}

// Run polls until ctx is cancelled, then waits for the CV processing it
// started. Updates handled since the last poll are confirmed by the next one,
// so after a restart they may be answered twice. Sign-ups in progress are kept
// in memory and lost on restart.
func (b *TelegramBot) Run(ctx context.Context) {
	defer b.running.Wait()
	var offset int64
	for ctx.Err() == nil {
		updates, err := b.bot.GetUpdates(ctx, offset, pollTimeoutSec)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := 5 * time.Second
			var tgErr *telegram.Error
			if errors.As(err, &tgErr) && tgErr.RetryAfter > 0 {
				wait = tgErr.RetryAfter
			}
			log.Printf("telegram bot: get updates: %v (retry in %s)", err, wait)
			sleepCtx(ctx, wait)
			continue
		}
		b.dropExpired()
		for _, u := range updates {
			offset = u.UpdateID + 1
			if err := b.handle(ctx, u.Message); err != nil {
				log.Printf("telegram bot: update %d: %v", u.UpdateID, err)
			}
		}
	}
}

func (b *TelegramBot) handle(ctx context.Context, msg *telegram.Message) error {
	// The account being registered is the sender's, so only private chats are served.
	if msg == nil || msg.From == nil || msg.Chat.Type != "private" {
		return nil
	}
	text := strings.TrimSpace(msg.Text)

	switch command(text) {
	case "/start":
		return b.start(ctx, msg)
	case "/cancel":
		delete(b.signups, msg.From.ID)
		return b.reply(ctx, msg, "Đã huỷ đăng ký. Gõ /start để bắt đầu lại.", telegram.RemoveKeyboard)
	}

	s := b.signups[msg.From.ID]
	if s == nil {
		return b.handleRegistered(ctx, msg)
	}
	s.expires = b.now().Add(signupTTL)
	if text == "" {
		return b.reply(ctx, msg, "Hãy trả lời bằng tin nhắn chữ nhé.", nil)
	}

	switch s.step {
	case stepName:
		return b.receiveName(ctx, msg, s, text)
	default:
		return b.receiveEmail(ctx, msg, s, text)
	}
}

func (b *TelegramBot) start(ctx context.Context, msg *telegram.Message) error {
	user, err := b.register.FindByTelegramID(ctx, msg.From.ID)
	switch {
	case err == nil:
		delete(b.signups, msg.From.ID)
		return b.reply(ctx, msg, fmt.Sprintf("Bạn đã đăng ký rồi.\nTên: %s\nEmail: %s\n\n%s",
			user.Name, user.Email, askCVText), telegram.RemoveKeyboard)
	case !errors.Is(err, repositories.ErrNotFound):
		return err
	}

	b.signups[msg.From.ID] = &signup{step: stepName, expires: b.now().Add(signupTTL)}

	// One tap answers with the Telegram name; typing another name works too.
	var markup telegram.ReplyMarkup = telegram.RemoveKeyboard
	if tgName := fullName(msg.From); tgName != "" {
		markup = &telegram.ReplyKeyboardMarkup{
			Keyboard:              [][]telegram.KeyboardButton{{{Text: tgName}}},
			ResizeKeyboard:        true,
			OneTimeKeyboard:       true,
			InputFieldPlaceholder: "Nhập họ tên",
		}
	}
	return b.reply(ctx, msg, "Chào mừng bạn! 👋\nTên bạn là gì?", markup)
}

func (b *TelegramBot) receiveName(ctx context.Context, msg *telegram.Message, s *signup, text string) error {
	name, err := services.NormalizeRegistrationName(text)
	if err != nil {
		return b.reply(ctx, msg, "Tên cần từ 1 đến 100 ký tự, bạn nhập lại nhé.", nil)
	}
	s.name, s.step = name, stepEmail
	return b.reply(ctx, msg, fmt.Sprintf("Chào %s!\nBây giờ hãy nhập email để kích hoạt tài khoản sử dụng hệ thống nhé!", name), telegram.RemoveKeyboard)
}

func (b *TelegramBot) receiveEmail(ctx context.Context, msg *telegram.Message, s *signup, text string) error {
	user, err := b.register.Register(ctx, msg.From.ID, s.name, text)
	switch {
	case err == nil:
		delete(b.signups, msg.From.ID)
		return b.reply(ctx, msg, fmt.Sprintf("✅ Kích hoạt tài khoản thành công!\nTên: %s\nEmail: %s\n\n%s",
			user.Name, user.Email, nextStepCVText), nil)
	case errors.Is(err, services.ErrInvalidEmail):
		return b.reply(ctx, msg, "Email chưa đúng định dạng (vd: ten@gmail.com), bạn nhập lại nhé.", nil)
	case errors.Is(err, services.ErrEmailRegistered):
		return b.reply(ctx, msg, "Email này đã được dùng cho tài khoản khác, bạn nhập email khác nhé.", nil)
	case errors.Is(err, services.ErrTelegramRegistered):
		delete(b.signups, msg.From.ID)
		return b.reply(ctx, msg, "Tài khoản Telegram này đã đăng ký rồi.", nil)
	default:
		// Keep the step: the next message retries with the same name.
		if rerr := b.reply(ctx, msg, "Có lỗi xảy ra, bạn gửi lại email sau ít phút nhé.", nil); rerr != nil {
			log.Printf("telegram bot: reply to %d: %v", msg.Chat.ID, rerr)
		}
		return err
	}
}

func (b *TelegramBot) reply(ctx context.Context, msg *telegram.Message, text string, markup telegram.ReplyMarkup) error {
	return b.bot.SendMessageTo(ctx, msg.Chat.ID, text, markup)
}

func (b *TelegramBot) dropExpired() {
	now := b.now()
	for id, s := range b.signups {
		if now.After(s.expires) {
			delete(b.signups, id)
		}
	}
}

// command returns the bot command of a message ("/start" for "/start@Bot payload"),
// or "" when the text is not a command.
func command(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	cmd, _, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return strings.ToLower(cmd)
}

func fullName(u *telegram.User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
