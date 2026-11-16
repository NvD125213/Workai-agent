package joboko

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/*.html are real Joboko pages (12/11/2026), cut down to the parts the
// parsers read, with SVG icons removed.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLocations(t *testing.T) {
	list := Locations()
	if len(list) != 66 || list[0].ID != LocationAll || list[1] != (Location{ID: 2, Slug: "ha-noi", Name: "Hà Nội"}) {
		t.Fatalf("Locations() = %d entries, first %+v %+v", len(list), list[0], list[1])
	}
	seen := map[int]bool{}
	for _, l := range list {
		if seen[l.ID] {
			t.Errorf("duplicate id %d", l.ID)
		}
		seen[l.ID] = true
	}

	for in, want := range map[string]int{
		"Hà Nội": 2, "ha noi": 2, "HA-NOI": 2, "Thành phố Hà Nội": 2, "HN": 2,
		"Hồ Chí Minh": 3, "TP.HCM": 3, "tp hcm": 3, "Sài Gòn": 3,
		"Đà Nẵng": 4, "Đắk Lắk": 20, "dak-lak": 20, "Thừa Thiên Huế": 57, "Huế": 57,
		"Bà Rịa - Vũng Tàu": 8, "Tỉnh Bắc Ninh": 12, "Nước Ngoài": 65, "": LocationAll,
	} {
		l, ok := LookupLocation(in)
		if !ok || l.ID != want {
			t.Errorf("LookupLocation(%q) = %+v, %v; want id %d", in, l, ok, want)
		}
	}
	if l, ok := LookupLocation("Paris"); ok {
		t.Errorf("LookupLocation(Paris) = %+v, want not found", l)
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL("golang developer", 1), "https://vn.joboko.com/jobs?q=golang+developer&l=2"; got != want {
		t.Errorf("page 1 = %s, want %s", got, want)
	}
	if err := c.SetLocation("TP.HCM"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.SearchURL("kế toán", 3), "https://vn.joboko.com/jobs?q=k%E1%BA%BF+to%C3%A1n&l=3&p=3"; got != want {
		t.Errorf("page 3 = %s, want %s", got, want)
	}
	if err := c.SetLocation("Atlantis"); err == nil {
		t.Error("unknown location must fail")
	}
}

func TestParseSearchPage(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(readFixture(t, "search.html")))
	if err != nil {
		t.Fatal(err)
	}
	if res.nextPage != 2 || len(res.Jobs) != 3 {
		t.Fatalf("next %d, jobs %d", res.nextPage, len(res.Jobs))
	}
	j := res.Jobs[0]
	if j.ID != "6818257" || j.Title != "Kỹ sư Computer Vision - Thu Nhập Upto 22 Triệu - Hà Nội" ||
		j.URL != "https://vn.joboko.com/viec-lam-ky-su-ai-xvi6818257" ||
		j.Company != "CÔNG TY CỔ PHẦN TẬP ĐOÀN GIẢI PHÁP SAO MAI" || j.Location != "Hà Nội" ||
		j.Salary != "16 - 22 triệu VND" || j.Deadline != "2026-12-02T17:11:00" ||
		!strings.HasPrefix(j.CompanyLogo, "https://u-vn.joboko.com/ComLogo/") || len(j.Highlights) != 3 {
		t.Errorf("job 0 = %+v", j)
	}
	if res.Jobs[1].ID != "6834332" || res.Jobs[1].Title != "PHP Developer" {
		t.Errorf("job 1 = %+v", res.Jobs[1])
	}
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail.html")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "PHP Developer" {
		t.Errorf("title = %q", d.Title)
	}
	// The consent popup's .name-company (VIETNIX) is on every page and must not be used.
	if d.Company != "Công ty Cổ phần VietISO" || d.CompanyURL != "https://vn.joboko.com/cong-ty-co-phan-vietiso-xci25830" {
		t.Errorf("company = %q %q", d.Company, d.CompanyURL)
	}
	want := Overview{
		Salary: "12 - 18 triệu VND", Location: "Hà Nội", Experience: "2 - 5 năm kinh nghiệm",
		Deadline: "2026-12-10", DatePosted: "2026-11-12", ValidThrough: "2026-12-10T08:33:00+07:00",
		EmploymentType: "FULL_TIME", Industries: "IT phần mềm, IT / Phần mềm / IOT / Điện tử viễn thông",
	}
	if d.Overview != want {
		t.Errorf("overview =\n%+v\nwant\n%+v", d.Overview, want)
	}
	if !strings.Contains(d.Description, "REST API") || strings.Contains(d.Description, "Mô tả công việc") {
		t.Errorf("description:\n%s", d.Description)
	}
	if !strings.Contains(d.Requirements, "PHP + MySQL 2-5 năm") {
		t.Errorf("requirements:\n%s", d.Requirements)
	}
	if !strings.Contains(d.Benefits, "Tháng lương 13") {
		t.Errorf("benefits:\n%s", d.Benefits)
	}
	if d.GeneralInfo["Thu nhập"] != "12 - 18 triệu VND" {
		t.Errorf("general info = %v", d.GeneralInfo)
	}
	if len(d.WorkLocations) != 1 || !strings.Contains(d.WorkLocations[0], "18 Tam Trinh") {
		t.Errorf("work locations = %v", d.WorkLocations)
	}
	if len(d.OtherSections) != 0 {
		t.Errorf("other sections = %v", d.OtherSections)
	}
}

// Some postings are one block without headings (.job-desc-full).
func TestParseJobDetailSingleBlock(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail-full.html")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title == "" || d.Overview.Experience != "3 - 4 năm kinh nghiệm" || d.Overview.Deadline != "2027-01-06" {
		t.Errorf("title %q, overview %+v", d.Title, d.Overview)
	}
	if !strings.HasPrefix(d.Description, "Mô tả công việc") || !strings.Contains(d.Description, "Microservices") {
		t.Errorf("description:\n%.300s", d.Description)
	}
}

// The experience row sometimes holds something else ("Toàn thời gian").
func TestParseJobDetailIgnoresBadExperience(t *testing.T) {
	page := strings.Replace(readFixture(t, "detail.html"), "2 - 5 năm kinh nghiệm", "Toàn thời gian", 1)
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if d.Overview.Experience != "" {
		t.Errorf("experience = %q, want empty", d.Overview.Experience)
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body><h1>404</h1></body></html>")); !errors.Is(err, ErrNotFound) {
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
	page1 := readFixture(t, "search.html")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs" || r.URL.Query().Get("l") != "2" || r.URL.Query().Get("q") != "developer" {
			t.Errorf("unexpected request %s", r.URL)
		}
		switch p := r.URL.Query().Get("p"); p {
		case "", "99": // past the end Joboko serves page 1 again
			w.Write([]byte(page1))
		case "2":
			last := strings.Replace(page1, `<a href="/jobs?q=developer&l=2&p=2"`, `<a class="hidden-x"`, 1)
			w.Write([]byte(last))
		default:
			t.Errorf("unexpected page %s", p)
		}
	}))
	ctx := context.Background()

	r1, err := c.FetchJobs(ctx, "developer", 1)
	if err != nil || r1.TotalPages != 2 || len(r1.Jobs) != 3 {
		t.Fatalf("page 1: %+v, %v", r1, err)
	}
	r2, err := c.FetchJobs(ctx, "developer", 2)
	if err != nil || r2.TotalPages != 2 {
		t.Errorf("page 2 (no next link): total %d, %v", r2.TotalPages, err)
	}
	r99, err := c.FetchJobs(ctx, "developer", 99)
	if err != nil || len(r99.Jobs) != 0 {
		t.Errorf("page 99 must be empty, got %d jobs, %v", len(r99.Jobs), err)
	}
}

func TestFetchJobDetails(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "-xvi6834332"):
			w.Write([]byte(readFixture(t, "detail.html")))
		case strings.HasSuffix(r.URL.Path, "-xvi1"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	jobs := []Job{
		{ID: "6834332", URL: "/viec-lam-php-developer-xvi6834332"},
		{ID: "1", URL: "https://vn.joboko.com/viec-lam-gone-xvi1"},
		{ID: "2", URL: "https://vn.joboko.com/viec-lam-blocked-xvi2"},
		{ID: "3", URL: "https://vn.joboko.com/viec-lam-never-fetched-xvi3"},
	}
	details, failures, err := c.FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 1 || len(failures) != 1 || failures[0].ID != "1" {
		t.Fatalf("details %d, failures %+v", len(details), failures)
	}
	if d := details[0]; d.ID != "6834332" || d.URL != "https://vn.joboko.com/viec-lam-php-developer-xvi6834332" {
		t.Errorf("detail = %s %s", d.ID, d.URL)
	}
}
