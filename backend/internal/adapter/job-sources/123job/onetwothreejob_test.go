package onetwothreejob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testdata/*.html are real 123job pages (12/11/2026), cut down to the parts the parsers
// read, with SVG icons removed.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLookupCity(t *testing.T) {
	for in, want := range map[string]int{
		"Hà Nội": 1, "ha noi": 1, "HN": 1, "Thành phố Hà Nội": 1,
		"Hồ Chí Minh": 63, "TP.HCM": 63, "Sài Gòn": 63,
		"Đà Nẵng": 2, "Huế": 14, "Bắc Kạn": 61, "Bà Rịa - Vũng Tàu": 9,
	} {
		if c, ok := LookupCity(in); !ok || c.ID != want {
			t.Errorf("LookupCity(%q) = %+v, %v; want id %d", in, c, ok, want)
		}
	}
	if _, ok := LookupCity("Atlantis"); ok {
		t.Error("unknown city must not be found")
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL("frontend developer", 1), "https://123job.vn/tuyen-dung?cityId=1&q=frontend+developer"; got != want {
		t.Errorf("page 1 = %s, want %s", got, want)
	}
	if err := c.SetCity("TP.HCM"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.SearchURL("kế toán", 2), "https://123job.vn/tuyen-dung?cityId=63&page=2&q=k%E1%BA%BF+to%C3%A1n"; got != want {
		t.Errorf("page 2 = %s, want %s", got, want)
	}
	if err := c.SetCity(""); err != nil || strings.Contains(c.SearchURL("x", 1), "cityId") {
		t.Errorf("every city must not send cityId (%v)", err)
	}
}

func TestParseSearchPage(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(readFixture(t, "search.html")))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalJobs != 1121 || !res.hasNext || len(res.Jobs) != 4 {
		t.Fatalf("total %d, next %v, jobs %d", res.TotalJobs, res.hasNext, len(res.Jobs))
	}
	j := res.Jobs[0]
	if j.ID != "5683942" || j.Title != "Backend Developer" ||
		j.URL != "https://123job.vn/viec-lam/backend-developer-4qQ4onVorX" ||
		j.Company != "Công Ty Cổ Phần Đầu Tư Quốc Tế Dglobal" || j.Location != "Hà Nội" ||
		j.Salary != "" || j.PostedAgo != "1 tuần trước" || j.CompanyLogo != "" {
		t.Errorf("job 0 = %+v", j)
	}
	if j := res.Jobs[2]; j.Salary != "10 - 15 triệu" || j.Company != "Bounce Game Studio" {
		t.Errorf("job 2 = %+v", j)
	}
	if j := res.Jobs[3]; !strings.HasPrefix(j.CompanyLogo, "https://cdn.123job.vn/") || j.Salary != "30 triệu" {
		t.Errorf("job 3 = %+v", j)
	}
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail.html")))
	if err != nil {
		t.Fatal(err)
	}
	want := Overview{
		Salary: "", Location: "Hà Nội", Experience: "2 năm", Education: "Không yêu cầu",
		WorkType: "Toàn thời gian", Industry: "IT phần mềm", Openings: "1",
		DatePosted: "2026-11-02", Deadline: "2026-12-10",
	}
	if d.Title != "Backend Developer" || d.Company != "CÔNG TY CỔ PHẦN ĐẦU TƯ QUỐC TẾ DGLOBAL" || d.Overview != want {
		t.Errorf("title %q, company %q, overview\n%+v\nwant\n%+v", d.Title, d.Company, d.Overview, want)
	}
	for name, text := range map[string]string{"description": d.Description, "requirements": d.Requirements, "benefits": d.Benefits} {
		if text == "" {
			t.Errorf("%s is empty", name)
		}
	}
	// The heading repeated as the first line is dropped, in full or shortened ("Yêu cầu").
	if strings.HasPrefix(d.Description, "Mô tả") || strings.HasPrefix(d.Requirements, "Yêu cầu\n") || strings.HasPrefix(d.Benefits, "Quyền lợi") {
		t.Errorf("headings left in:\n%.80s\n%.80s\n%.80s", d.Description, d.Requirements, d.Benefits)
	}
	if !strings.Contains(d.Requirements, "Spring Boot") {
		t.Errorf("requirements:\n%s", d.Requirements)
	}
	if d.Info["Kinh nghiệm"] != "OccupationalExperienceRequirements, 24" || d.Info["Giới tính"] != "Không yêu cầu" {
		t.Errorf("info (as displayed) = %v", d.Info)
	}
}

func TestParseJobDetailSalaryAndMonths(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail-salary.html")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Overview.Salary != "10 - 15 triệu" || d.Overview.Experience != "3 tháng" || d.Overview.Deadline != "2026-12-12" {
		t.Errorf("overview = %+v", d.Overview)
	}
}

// A closed posting: "Hết hạn" badge in the title (real markup).
func TestParseJobDetailExpired(t *testing.T) {
	page := regexp.MustCompile(`Backend Developer\s*</h1>`).ReplaceAllString(readFixture(t, "detail.html"),
		`Backend Developer <span class="inline-flex items-center px-2 py-0.5 text-[13px]">Hết hạn</span> </h1>`)
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Backend Developer" || !d.Overview.Expired {
		t.Errorf("title %q, expired %v", d.Title, d.Overview.Expired)
	}
	if d2, _ := ParseJobDetail(strings.NewReader(readFixture(t, "detail.html"))); d2.Overview.Expired {
		t.Error("an open posting must not be expired")
	}
}

func TestExperienceAndEducationText(t *testing.T) {
	for in, want := range map[string]string{
		"OccupationalExperienceRequirements, 24": "2 năm",
		"OccupationalExperienceRequirements, 36": "3 năm",
		"OccupationalExperienceRequirements, 6":  "6 tháng",
		"OccupationalExperienceRequirements, 0":  "Không yêu cầu kinh nghiệm",
		"Trên 2 năm":                             "Trên 2 năm",
	} {
		if got := experienceText(in); got != want {
			t.Errorf("experienceText(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"EducationalOccupationalCredential, bachelor degree": "Đại học",
		"Không yêu cầu": "Không yêu cầu",
	} {
		if got := educationText(in); got != want {
			t.Errorf("educationText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body><p>404</p></body></html>")); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client(), srv.URL)
	c.SetRequestDelay(0, 0)
	c.retryBase = time.Millisecond
	return c
}

func TestFetchJobsPaging(t *testing.T) {
	page := readFixture(t, "search.html")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tuyen-dung" || r.URL.Query().Get("cityId") != "1" {
			t.Errorf("unexpected request %s", r.URL)
		}
		switch r.URL.Query().Get("page") {
		case "3": // last page: no next link
			w.Write([]byte(strings.Replace(page, `rel="next"`, `rel="nofollow"`, 1)))
			return
		case "999": // past the end the site answers 404
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(page))
	}))
	r1, err := c.FetchJobs(context.Background(), "developer", 1)
	if err != nil || r1.TotalPages != 38 { // ceil(1121 / 30)
		t.Errorf("page 1: pages %d, %v", r1.TotalPages, err)
	}
	r3, err := c.FetchJobs(context.Background(), "developer", 3)
	if err != nil || r3.TotalPages != 3 {
		t.Errorf("page 3 without next link (and a short page): pages %d, %v", r3.TotalPages, err)
	}
	r999, err := c.FetchJobs(context.Background(), "developer", 999)
	if err != nil || len(r999.Jobs) != 0 {
		t.Errorf("page 999 (404) must be an empty last page, got %+v, %v", r999, err)
	}
}

func TestFetchJobDetails(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "-v91BPZAxrW"):
			w.Write([]byte(readFixture(t, "detail-salary.html")))
		case strings.HasSuffix(r.URL.Path, "-gone"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	jobs := []Job{
		{ID: "5683188", URL: "https://123job.vn/viec-lam/unity-developer-v91BPZAxrW", Salary: "10 - 15 triệu"},
		{ID: "1", URL: "https://123job.vn/viec-lam/x-gone"},
		{ID: "2", URL: "https://123job.vn/viec-lam/x-blocked"},
		{ID: "3", URL: "https://123job.vn/viec-lam/x-never"},
	}
	details, failures, err := c.FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 1 || len(failures) != 1 || failures[0].ID != "1" {
		t.Fatalf("details %d, failures %+v", len(details), failures)
	}
	if d := details[0]; d.ID != "5683188" || d.URL != "https://123job.vn/viec-lam/unity-developer-v91BPZAxrW" {
		t.Errorf("detail = %s %s", d.ID, d.URL)
	}
}
