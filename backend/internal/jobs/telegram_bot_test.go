package jobs

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"notification-job-mb/internal/adapter/telegram"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

type sentMessage struct {
	chatID int64
	text   string
	markup telegram.ReplyMarkup
	html   bool
}

type fakeUpdates struct{ sent []sentMessage }

func (f *fakeUpdates) GetUpdates(context.Context, int64, int) ([]telegram.Update, error) {
	return nil, nil
}

func (f *fakeUpdates) SendMessageTo(_ context.Context, chatID int64, text string, markup telegram.ReplyMarkup) error {
	f.sent = append(f.sent, sentMessage{chatID: chatID, text: text, markup: markup})
	return nil
}

func (f *fakeUpdates) SendHTMLTo(_ context.Context, chatID int64, html string) error {
	f.sent = append(f.sent, sentMessage{chatID: chatID, text: html, html: true})
	return nil
}

func (f *fakeUpdates) DownloadFile(context.Context, string, int64) ([]byte, error) {
	return []byte("cv bytes"), nil
}

func (f *fakeUpdates) last() sentMessage { return f.sent[len(f.sent)-1] }

func (f *fakeUpdates) texts() string {
	var all []string
	for _, m := range f.sent {
		all = append(all, m.text)
	}
	return strings.Join(all, "\n---\n")
}

// memUsers is an in-memory user store under the real TelegramRegisterService.
type memUsers map[int64]*models.User

func (m memUsers) FindByTelegramID(_ context.Context, id int64) (*models.User, error) {
	if u, ok := m[id]; ok {
		return u, nil
	}
	return nil, repositories.ErrNotFound
}

func (m memUsers) Create(_ context.Context, u *models.User) error {
	for _, other := range m {
		if other.Email == u.Email {
			return repositories.ErrEmailTaken
		}
	}
	u.ID = uint(len(m) + 1)
	m[*u.TelegramID] = u
	return nil
}

// fakeCV analyzes at once with the result it is given.
type fakeCV struct {
	parseErr    error
	failMessage string // analysis ends FAILED with this error
	parsedFile  string
}

func (f *fakeCV) ParseCV(_ context.Context, _ uint, filename string, _ io.Reader) (*models.UserProfile, error) {
	f.parsedFile = filename
	return &models.UserProfile{}, f.parseErr
}

func (f *fakeCV) StartAnalysis(context.Context, uint) (*models.CVAnalysis, error) {
	return &models.CVAnalysis{ID: 3, Status: models.AnalysisStatusQueued}, nil
}

func (f *fakeCV) GetAnalysis(context.Context, uint, uint) (*services.AnalysisView, error) {
	if f.failMessage != "" {
		return &services.AnalysisView{Analysis: &models.CVAnalysis{
			ID: 3, Status: models.AnalysisStatusFailed, Error: f.failMessage}}, nil
	}
	return &services.AnalysisView{
		Analysis: &models.CVAnalysis{ID: 3, Status: models.AnalysisStatusDone},
		Profile:  &models.UserProfile{CurrentTitle: "Backend Developer", ExpYears: 2},
		Skills:   []models.UserSkill{{SkillName: "go"}, {SkillName: "docker"}},
	}, nil
}

type fakeFinder struct {
	matches  []models.AIMatch
	statuses []string
	started  StartRequest
}

func (f *fakeFinder) Start(_ context.Context, _ uint, req StartRequest) (*models.JobSearch, error) {
	f.started = req
	return &models.JobSearch{ID: 7, Status: models.SearchStatusQueued}, nil
}

// Get goes through statuses, one per call, and stays DONE after them.
func (f *fakeFinder) Get(context.Context, uint, uint) (*SearchView, error) {
	status := models.SearchStatusDone
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	v := &SearchView{Search: &models.JobSearch{ID: 7, Status: status}}
	if status == models.SearchStatusDone {
		v.Search.JobsFound, v.Search.MatchesFound, v.Matches = 184, len(f.matches), f.matches
	}
	return v, nil
}

func privateMsg(from int64, text string) *telegram.Message {
	return &telegram.Message{
		From: &telegram.User{ID: from, FirstName: "Văn", LastName: "Đức"},
		Chat: telegram.Chat{ID: from, Type: "private"},
		Text: text,
	}
}

func docMsg(from int64, name string) *telegram.Message {
	m := privateMsg(from, "")
	m.Document = &telegram.Document{FileID: "f1", FileName: name, FileSize: 1000}
	return m
}

func newTestBot() (*TelegramBot, *fakeUpdates, memUsers, *fakeCV, *fakeFinder) {
	api, users, cv, finder := &fakeUpdates{}, memUsers{}, &fakeCV{}, &fakeFinder{}
	b := NewTelegramBot(api, services.NewTelegramRegisterService(users), cv, finder,
		TelegramCVConfig{AnalysisPoll: time.Millisecond, SearchPoll: time.Millisecond})
	return b, api, users, cv, finder
}

func match(rank int, title string) models.AIMatch {
	return models.AIMatch{
		Rank: rank, Score: 90 - float64(rank),
		Details: datatypes.JSON(`{"matched_skills":["go","docker"]}`),
		Job: &models.Job{
			Title: title, Company: "Công ty A&B", URL: fmt.Sprintf("https://example.com/job/%d?a=1&b=2", rank),
			RawSalary: "15 - 20 triệu", Source: &models.Source{Name: "topcv"},
		},
	}
}

func TestTelegramBotSignup(t *testing.T) {
	b, api, users, _, _ := newTestBot()
	ctx := context.Background()
	send := func(text string) sentMessage {
		t.Helper()
		if err := b.handle(ctx, privateMsg(1, text)); err != nil {
			t.Fatal(err)
		}
		return api.last()
	}

	m := send("/start")
	kb, ok := m.markup.(*telegram.ReplyKeyboardMarkup)
	if !strings.Contains(m.text, "Tên bạn là gì?") || !ok || kb.Keyboard[0][0].Text != "Văn Đức" {
		t.Fatalf("/start reply = %+v", m)
	}

	m = send("Nguyễn Văn Đức")
	if !strings.Contains(m.text, "email để kích hoạt tài khoản") || m.markup != telegram.RemoveKeyboard {
		t.Fatalf("name reply = %+v", m)
	}

	m = send("duc@gmail")
	if !strings.Contains(m.text, "chưa đúng định dạng") {
		t.Fatalf("bad email reply = %+v", m)
	}

	m = send("Duc@Gmail.com")
	if !strings.Contains(m.text, "thành công") || !strings.Contains(m.text, nextStepCVText) {
		t.Fatalf("email reply = %+v", m)
	}
	if u := users[1]; u == nil || u.Name != "Nguyễn Văn Đức" || u.Email != "duc@gmail.com" {
		t.Fatalf("user = %+v", users[1])
	}

	m = send("/start")
	if !strings.Contains(m.text, "đã đăng ký rồi") || !strings.Contains(m.text, "Gửi CV") {
		t.Fatalf("second /start reply = %+v", m)
	}
	if len(b.signups) != 0 {
		t.Errorf("signups left: %v", b.signups)
	}
}

func TestTelegramBotEmailTaken(t *testing.T) {
	b, api, users, _, _ := newTestBot()
	users[9] = &models.User{Email: "taken@example.com"}
	ctx := context.Background()
	for _, text := range []string{"/start", "A", "taken@example.com"} {
		if err := b.handle(ctx, privateMsg(1, text)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(api.last().text, "đã được dùng") || b.signups[1] == nil {
		t.Fatalf("reply = %+v, signup = %+v", api.last(), b.signups[1])
	}
}

func TestTelegramBotIgnoresAndExpires(t *testing.T) {
	b, api, _, _, _ := newTestBot()
	ctx := context.Background()

	group := privateMsg(1, "/start")
	group.Chat.Type = "group"
	if err := b.handle(ctx, group); err != nil || len(api.sent) != 0 {
		t.Fatalf("group message answered: err=%v sent=%d", err, len(api.sent))
	}

	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	_ = b.handle(ctx, privateMsg(1, "/start"))
	now = now.Add(signupTTL + time.Second)
	b.dropExpired()
	_ = b.handle(ctx, privateMsg(1, "Văn Đức"))
	if !strings.Contains(api.last().text, "/start") {
		t.Fatalf("expired signup reply = %+v", api.last())
	}
}

func TestTelegramBotCVFlow(t *testing.T) {
	b, api, users, cv, finder := newTestBot()
	users[1] = &models.User{ID: 5, Name: "A", Email: "a@b.co"}
	finder.statuses = []string{models.SearchStatusCrawling, models.SearchStatusCrawling,
		models.SearchStatusRanking, models.SearchStatusRanking}
	for i := 1; i <= 12; i++ {
		finder.matches = append(finder.matches, match(i, fmt.Sprintf("Go Developer <%d>", i)))
	}

	if err := b.handle(context.Background(), docMsg(1, "cv.PDF")); err != nil {
		t.Fatal(err)
	}
	b.running.Wait()

	if cv.parsedFile != "cv.PDF" || finder.started.AnalysisID != 3 {
		t.Fatalf("parsed %q, search %+v", cv.parsedFile, finder.started)
	}
	// Progress messages, in order, each once.
	all := api.texts()
	pos := 0
	for _, want := range []string{"Đã nhận CV! Mình đang phân tích CV", "Backend Developer", "Kỹ năng: go, docker",
		"Quá trình sẽ mất 10-15 phút, xin hãy chờ", "đang chấm điểm", "Top 10 việc làm phù hợp nhất",
		"Từ 184 tin tuyển dụng, có 12 tin đạt"} {
		i := strings.Index(all[pos:], want)
		if i < 0 {
			t.Fatalf("messages lack %q after position %d:\n%s", want, pos, all)
		}
		pos += i
	}
	if n := strings.Count(all, rankingText); n != 1 {
		t.Errorf("ranking message sent %d times", n)
	}
	var jobs string
	for _, m := range api.sent {
		if m.html {
			jobs += m.text
			if len(m.text) > maxHTMLBytes {
				t.Errorf("html message of %d bytes", len(m.text))
			}
		}
	}
	if !strings.Contains(jobs, "<b>10. ") || strings.Contains(jobs, "<b>11. ") {
		t.Errorf("not the top 10:\n%s", jobs)
	}
	if !strings.Contains(jobs, `<a href="https://example.com/job/1?a=1&amp;b=2">Go Developer &lt;1&gt;</a>`) ||
		!strings.Contains(jobs, "Công ty A&amp;B · topcv") || !strings.Contains(jobs, "✅ Khớp: go, docker") {
		t.Errorf("job not rendered/escaped:\n%s", jobs)
	}
	if b.isBusy(1) {
		t.Error("user still busy after processing")
	}
}

func TestTelegramBotCVRejected(t *testing.T) {
	b, api, users, cv, finder := newTestBot()
	users[1] = &models.User{ID: 5}
	cv.parseErr = &services.CVRejectedError{Message: "Nội dung file không giống CV."}

	_ = b.handle(context.Background(), docMsg(1, "menu.pdf"))
	b.running.Wait()
	if m := api.last(); m.text != "❌ Nội dung file không giống CV." {
		t.Fatalf("reply = %+v", m)
	}
	if finder.started.AnalysisID != 0 {
		t.Error("search started for a rejected file")
	}

	// Rejected by the LLM check after parsing: the analysis ends FAILED with the AI service's reason.
	cv.parseErr, cv.failMessage = nil, "Không tìm thấy chức danh, kinh nghiệm hay kỹ năng nào trong file."
	_ = b.handle(context.Background(), docMsg(1, "menu.pdf"))
	b.running.Wait()
	if m := api.last(); !strings.Contains(m.text, "Không tìm thấy chức danh") {
		t.Fatalf("reply = %+v", m)
	}
}

func TestTelegramBotCVChecks(t *testing.T) {
	b, api, users, cv, _ := newTestBot()
	ctx := context.Background()

	_ = b.handle(ctx, docMsg(2, "cv.pdf"))
	if !strings.Contains(api.last().text, "/start") {
		t.Errorf("unregistered user: %+v", api.last())
	}

	users[1] = &models.User{ID: 5}
	_ = b.handle(ctx, docMsg(1, "photo.png"))
	if !strings.Contains(api.last().text, "PDF, DOCX") {
		t.Errorf("png: %+v", api.last())
	}

	big := docMsg(1, "cv.pdf")
	big.Document.FileSize = 11 << 20
	_ = b.handle(ctx, big)
	if !strings.Contains(api.last().text, "10 MB") {
		t.Errorf("big file: %+v", api.last())
	}

	photo := privateMsg(1, "")
	photo.Photo = []telegram.PhotoSize{{FileID: "p"}}
	_ = b.handle(ctx, photo)
	if !strings.Contains(api.last().text, "dạng ảnh") {
		t.Errorf("photo: %+v", api.last())
	}

	b.acquire(1)
	_ = b.handle(ctx, docMsg(1, "cv.pdf"))
	if api.last().text != busyCVText || cv.parsedFile != "" {
		t.Errorf("busy: %+v, parsed %q", api.last(), cv.parsedFile)
	}
}

func TestRenderTopJobs(t *testing.T) {
	none := renderTopJobs(&SearchView{Search: &models.JobSearch{JobsFound: 50}}, topJobs)
	if len(none) != 1 || !strings.Contains(none[0], "50 tin") {
		t.Errorf("no matches: %v", none)
	}

	// Long titles and URLs: split into several messages, none over the limit, no job lost.
	var ms []models.AIMatch
	for i := 1; i <= 10; i++ {
		m := match(i, strings.Repeat("Rất dài ", 30))
		m.Job.URL = "https://example.com/" + strings.Repeat("x", 900)
		ms = append(ms, m)
	}
	out := renderTopJobs(&SearchView{Search: &models.JobSearch{}, Matches: ms}, topJobs)
	if len(out) < 2 {
		t.Fatalf("expected a split, got %d message(s)", len(out))
	}
	joined := strings.Join(out, "")
	for i, m := range out {
		if len(m) > maxHTMLBytes {
			t.Errorf("message %d has %d bytes", i, len(m))
		}
	}
	for i := 1; i <= 10; i++ {
		if !strings.Contains(joined, fmt.Sprintf("<b>%d. ", i)) {
			t.Errorf("job %d missing", i)
		}
	}
}

func TestAnalysisFailureText(t *testing.T) {
	if got := analysisFailureText("the AI service failed; try again later"); !strings.Contains(got, "AI") {
		t.Errorf("got %q", got)
	}
	if got := analysisFailureText("Không đọc được chữ trong CV."); got != "Không đọc được chữ trong CV." {
		t.Errorf("got %q", got)
	}
}

func TestCommand(t *testing.T) {
	for text, want := range map[string]string{
		"/start": "/start", "/START@MyBot": "/start", "/start abc": "/start", "start": "", "": "",
	} {
		if got := command(text); got != want {
			t.Errorf("command(%q) = %q, want %q", text, got, want)
		}
	}
}
