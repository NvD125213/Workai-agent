package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"notification-job-mb/internal/adapter/telegram"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

const (
	nextStepCVText = "Tiếp theo, hãy gửi CV để hệ thống tìm kiếm job phù hợp cho bạn nhé."
	askCVText      = "Gửi CV (file PDF, DOCX, TXT hoặc MD, tối đa 10 MB) để hệ thống tìm job phù hợp cho bạn nhé."
	busyCVText     = "CV trước của bạn vẫn đang được xử lý, có kết quả mình sẽ nhắn ngay nhé."
	retryCVText    = "Có lỗi xảy ra, bạn gửi lại CV sau ít phút nhé."
	crawlingText   = "🌐 Đang cào job từ các trang tuyển dụng (TopCV, ITviec, JobsGO, Indeed, VietnamWorks, CareerViet, Joboko, Việc Làm 24h, 123job, CareerLink...) " +
		"rồi chấm điểm độ phù hợp với CV của bạn.\n⏳ Quá trình sẽ mất 10-15 phút, xin hãy chờ."
	rankingText = "🧮 Đã cào xong, đang chấm điểm độ phù hợp của từng job với CV của bạn..."

	// maxTelegramCVBytes: the limit of POST /parse-cv and of the AI service.
	maxTelegramCVBytes = 10 << 20
	topJobs            = 10
	// maxHTMLBytes keeps a message under Telegram's 4096 characters with room to spare.
	maxHTMLBytes = 3800
)

// cvExtensions are the files the AI service reads (.doc gets its explanation from it).
var cvExtensions = map[string]bool{".pdf": true, ".docx": true, ".doc": true, ".txt": true, ".md": true}

var errWaitTimeout = errors.New("telegram bot: gave up waiting")

type TelegramCVConfig struct {
	// ExpFilter is passed to the job search: keep only jobs in the experience
	// range of the CV and rank by salary (see FindJobs.Start).
	ExpFilter bool
	// How often the analysis and the search are checked, and for how long.
	AnalysisPoll, SearchPoll time.Duration
	AnalysisWait, SearchWait time.Duration
}

func (c *TelegramCVConfig) applyDefaults() {
	if c.AnalysisPoll <= 0 {
		c.AnalysisPoll = 5 * time.Second
	}
	if c.SearchPoll <= 0 {
		c.SearchPoll = 20 * time.Second
	}
	if c.AnalysisWait <= 0 {
		c.AnalysisWait = 15 * time.Minute
	}
	if c.SearchWait <= 0 {
		// Crawling 3 pages of 5 sources with the anti-blocking delays can take half an hour.
		c.SearchWait = 90 * time.Minute
	}
}

type cvAnalyzer interface {
	ParseCV(ctx context.Context, userID uint, filename string, file io.Reader) (*models.UserProfile, error)
	StartAnalysis(ctx context.Context, userID uint) (*models.CVAnalysis, error)
	GetAnalysis(ctx context.Context, userID, id uint) (*services.AnalysisView, error)
}

type jobFinder interface {
	Start(ctx context.Context, userID uint, req StartRequest) (*models.JobSearch, error)
	Get(ctx context.Context, userID, id uint) (*SearchView, error)
}

// handleRegistered answers a message outside the sign-up conversation.
func (b *TelegramBot) handleRegistered(ctx context.Context, msg *telegram.Message) error {
	user, err := b.register.FindByTelegramID(ctx, msg.From.ID)
	if errors.Is(err, repositories.ErrNotFound) {
		return b.reply(ctx, msg, "Gõ /start để đăng ký nhận thông báo việc làm.", nil)
	}
	if err != nil {
		return err
	}

	switch {
	case msg.Document != nil:
		return b.receiveCV(ctx, msg, user)
	case len(msg.Photo) > 0:
		return b.reply(ctx, msg, "Mình chưa đọc được CV dạng ảnh. Hãy gửi CV dưới dạng file PDF hoặc DOCX nhé.", nil)
	case b.isBusy(msg.From.ID):
		return b.reply(ctx, msg, busyCVText, nil)
	default:
		return b.reply(ctx, msg, askCVText, nil)
	}
}

// receiveCV checks the file and starts its processing in the background:
// the analysis and the search take up to 15 minutes, updates keep flowing.
func (b *TelegramBot) receiveCV(ctx context.Context, msg *telegram.Message, user *models.User) error {
	doc := *msg.Document
	if ext := strings.ToLower(filepath.Ext(doc.FileName)); !cvExtensions[ext] {
		return b.reply(ctx, msg, "Mình chỉ đọc được CV dạng PDF, DOCX, TXT hoặc MD. Bạn gửi lại đúng định dạng nhé.", nil)
	}
	if doc.FileSize > maxTelegramCVBytes {
		return b.reply(ctx, msg, "File CV lớn hơn 10 MB, bạn gửi file nhỏ hơn nhé.", nil)
	}
	if !b.acquire(msg.From.ID) {
		return b.reply(ctx, msg, busyCVText, nil)
	}
	b.running.Go(func() {
		defer b.release(msg.From.ID)
		b.processCV(ctx, msg.Chat.ID, user.ID, doc)
	})
	return nil
}

// processCV runs the same steps as the HTTP API: parse (which rejects files
// that are not a CV), analyze, search, then sends the best jobs.
func (b *TelegramBot) processCV(ctx context.Context, chatID int64, userID uint, doc telegram.Document) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("telegram bot: cv of user %d: panic: %v", userID, r)
		}
	}()
	say := func(text string) {
		if err := b.bot.SendMessageTo(ctx, chatID, text, nil); err != nil && ctx.Err() == nil {
			log.Printf("telegram bot: send to %d: %v", chatID, err)
		}
	}
	fail := func(step string, err error, text string) {
		if ctx.Err() != nil {
			return // shutting down; the user resends the CV
		}
		log.Printf("telegram bot: cv of user %d: %s: %v", userID, step, err)
		say(text)
	}

	say("📄 Đã nhận CV! Mình đang phân tích CV của bạn, mất khoảng 1-2 phút...")
	data, err := b.bot.DownloadFile(ctx, doc.FileID, maxTelegramCVBytes)
	if errors.Is(err, telegram.ErrFileTooLarge) {
		say("File CV lớn hơn 10 MB, bạn gửi file nhỏ hơn nhé.")
		return
	}
	if err != nil {
		fail("download", err, "Mình không tải được file, bạn gửi lại CV nhé.")
		return
	}

	if _, err := b.cv.ParseCV(ctx, userID, doc.FileName, bytes.NewReader(data)); err != nil {
		var rejected *services.CVRejectedError
		if errors.As(err, &rejected) {
			say("❌ " + rejected.Message)
			return
		}
		fail("parse", err, aiFailureText(err))
		return
	}

	a, err := b.cv.StartAnalysis(ctx, userID)
	if err != nil {
		fail("start analysis", err, retryCVText)
		return
	}

	view, err := b.waitAnalysis(ctx, userID, a.ID)
	switch {
	case errors.Is(err, errWaitTimeout):
		fail("analysis", err, "Phân tích CV lâu hơn bình thường, bạn gửi lại CV sau ít phút nhé.")
		return
	case err != nil:
		fail("analysis", err, retryCVText)
		return
	case view.Analysis.Status == models.AnalysisStatusFailed:
		say("❌ Không phân tích được CV: " + analysisFailureText(view.Analysis.Error))
		return
	case view.Outdated || view.Profile == nil:
		fail("analysis", errors.New("analysis outdated"), retryCVText)
		return
	}
	say(profileSummary(view))

	s, err := b.finder.Start(ctx, userID, StartRequest{AnalysisID: a.ID, ExpFilter: b.cfg.ExpFilter})
	switch {
	case errors.Is(err, ErrSearchInProgress):
		say("Bạn đang có một lượt tìm việc chưa xong, đợi lượt đó xong rồi gửi lại CV nhé.")
		return
	case errors.Is(err, ErrNoKeyword):
		say("Mình chưa xác định được ngành nghề từ CV nên chưa tìm việc được. Bạn kiểm tra lại CV nhé.")
		return
	case err != nil:
		fail("start search", err, retryCVText)
		return
	}
	say(crawlingText)

	sv, err := b.waitSearch(ctx, userID, s.ID, func(status string) {
		if status == models.SearchStatusRanking {
			say(rankingText)
		}
	})
	switch {
	case errors.Is(err, errWaitTimeout):
		fail("search", err, "Tìm việc lâu hơn bình thường, bạn gửi lại CV sau nhé.")
		return
	case err != nil:
		fail("search", err, retryCVText)
		return
	case sv.Search.Status == models.SearchStatusFailed && strings.HasPrefix(sv.Search.Error, ScoreTimeoutError):
		fail("search", errors.New(sv.Search.Error),
			"⌛ Cào được quá nhiều job nên chấm điểm lâu hơn giới hạn cho phép, bạn gửi lại CV sau ít phút nhé.")
		return
	case sv.Search.Status == models.SearchStatusFailed:
		fail("search", errors.New(sv.Search.Error), "❌ Tìm việc thất bại, bạn gửi lại CV sau ít phút nhé.")
		return
	}
	for _, m := range renderTopJobs(sv, topJobs) {
		if err := b.bot.SendHTMLTo(ctx, chatID, m); err != nil {
			log.Printf("telegram bot: send jobs to %d: %v", chatID, err)
			return
		}
	}
}

func (b *TelegramBot) waitAnalysis(ctx context.Context, userID, id uint) (*services.AnalysisView, error) {
	return poll(ctx, b.cfg.AnalysisPoll, b.cfg.AnalysisWait, func() (*services.AnalysisView, bool, error) {
		v, err := b.cv.GetAnalysis(ctx, userID, id)
		if err != nil {
			return nil, false, err
		}
		st := v.Analysis.Status
		return v, st == models.AnalysisStatusDone || st == models.AnalysisStatusFailed, nil
	})
}

// waitSearch calls onStatus each time the search reaches a new status.
func (b *TelegramBot) waitSearch(ctx context.Context, userID, id uint, onStatus func(status string)) (*SearchView, error) {
	last := ""
	return poll(ctx, b.cfg.SearchPoll, b.cfg.SearchWait, func() (*SearchView, bool, error) {
		v, err := b.finder.Get(ctx, userID, id)
		if err != nil {
			return nil, false, err
		}
		st := v.Search.Status
		if st != last {
			last = st
			onStatus(st)
		}
		return v, st == models.SearchStatusDone || st == models.SearchStatusFailed, nil
	})
}

// poll calls check every interval until it reports done, fails, or wait ends.
func poll[T any](ctx context.Context, interval, wait time.Duration, check func() (T, bool, error)) (T, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		v, done, err := check()
		if err != nil || done {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-deadline.C:
			return v, errWaitTimeout
		case <-tick.C:
		}
	}
}

func (b *TelegramBot) acquire(telegramID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy[telegramID] {
		return false
	}
	b.busy[telegramID] = true
	return true
}

func (b *TelegramBot) release(telegramID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.busy, telegramID)
}

func (b *TelegramBot) isBusy(telegramID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy[telegramID]
}

func aiFailureText(err error) string {
	if errors.Is(err, services.ErrAIUnavailable) {
		return "Hệ thống AI đang bận hoặc gặp sự cố, bạn gửi lại CV sau ít phút nhé."
	}
	return retryCVText
}

// analysisFailureText turns cv_analyses.error into a message for the user:
// the service's own messages are in English, the AI service's refusals
// (not a CV, scanned image...) are already in Vietnamese.
func analysisFailureText(msg string) string {
	switch {
	case strings.HasPrefix(msg, "the AI service failed"):
		return "hệ thống AI đang gặp sự cố, bạn gửi lại CV sau ít phút nhé."
	case strings.HasPrefix(msg, "server stopped"):
		return "máy chủ vừa khởi động lại, bạn gửi lại CV nhé."
	case strings.HasPrefix(msg, "a newer CV was uploaded"):
		return "bạn vừa gửi CV khác trong lúc phân tích, mình sẽ dùng CV mới nhất nhé."
	case msg == "" || msg == "internal error":
		return "có lỗi xảy ra, bạn gửi lại CV sau ít phút nhé."
	default:
		return msg
	}
}

func profileSummary(v *services.AnalysisView) string {
	p := v.Profile
	var sb strings.Builder
	sb.WriteString("✅ Đã phân tích xong CV của bạn:")
	if p.CurrentTitle != "" {
		fmt.Fprintf(&sb, "\n• Chức danh: %s", p.CurrentTitle)
	}
	fmt.Fprintf(&sb, "\n• Kinh nghiệm: %d năm", p.ExpYears)
	if len(v.Skills) > 0 {
		names := make([]string, 0, 8)
		for _, s := range v.Skills {
			if len(names) == cap(names) {
				break
			}
			names = append(names, s.SkillName)
		}
		fmt.Fprintf(&sb, "\n• Kỹ năng: %s", strings.Join(names, ", "))
	}
	return sb.String()
}

// renderTopJobs formats the best limit matches as Telegram HTML messages,
// split so that each stays under maxHTMLBytes.
func renderTopJobs(v *SearchView, limit int) []string {
	s := v.Search
	if len(v.Matches) == 0 {
		return []string{fmt.Sprintf("😔 Mình đã quét %d tin tuyển dụng nhưng chưa có job nào đủ phù hợp với CV của bạn. "+
			"Bạn có thể gửi lại CV sau để tìm lại nhé.", s.JobsFound)}
	}

	matches := v.Matches
	if len(matches) > limit {
		matches = matches[:limit]
	}
	head := fmt.Sprintf("🎯 <b>Top %d việc làm phù hợp nhất với bạn</b>\nTừ %d tin tuyển dụng, có %d tin đạt yêu cầu.\n",
		len(matches), s.JobsFound, s.MatchesFound)

	var out []string
	cur := head
	for i, m := range matches {
		block := jobBlock(i+1, m)
		if len(cur)+len(block) > maxHTMLBytes {
			out = append(out, cur)
			cur = ""
		}
		cur += block
	}
	return append(out, cur)
}

func jobBlock(rank int, m models.AIMatch) string {
	var title, company, source, url, salary string
	if j := m.Job; j != nil {
		title, company, url, salary = j.Title, j.Company, j.URL, j.RawSalary
		if j.Source != nil {
			source = j.Source.Name
		}
	}
	title = html.EscapeString(truncateRunes(firstNonEmpty(title, "(không có tiêu đề)"), 120))
	if url != "" && len(url) <= 1000 {
		title = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(url), title)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n<b>%d. %s</b>\n", rank, title)
	if place := strings.Join(nonEmpty(truncateRunes(company, 80), source), " · "); place != "" {
		fmt.Fprintf(&sb, "🏢 %s\n", html.EscapeString(place))
	}
	line := fmt.Sprintf("⭐ %.1f/100", m.Score)
	if salary = strings.TrimSpace(salary); salary != "" {
		line = "💰 " + html.EscapeString(truncateRunes(salary, 60)) + " · " + line
	}
	sb.WriteString(line + "\n")

	var d matchDetails
	if json.Unmarshal(m.Details, &d) == nil && len(d.MatchedSkills) > 0 {
		skills := d.MatchedSkills
		if len(skills) > 5 {
			skills = skills[:5]
		}
		fmt.Fprintf(&sb, "✅ Khớp: %s\n", html.EscapeString(truncateRunes(strings.Join(skills, ", "), 120)))
	}
	return sb.String()
}

func nonEmpty(values ...string) []string {
	out := values[:0]
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
