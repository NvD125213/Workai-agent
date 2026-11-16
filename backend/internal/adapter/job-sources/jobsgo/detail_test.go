package jobsgo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// testdata/job-detail.html is a real page of
// https://jobsgo.vn/viec-lam/lap-trinh-vien-full-stack-java-developer-onsite-thu-nhap-up-to-26-trieu-nghi-thu-bay-chu-nhat-29391229498.html
// with scripts (except JSON-LD), styles, icons, header and footer removed.
func readFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/job-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t)))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}

	minS, maxS, months := 20000000.0, 26000000.0, 12
	industry := "DevOps - Cloud, Lập Trình - Phát Triển Phần Mềm, Fintech, Quản Lý Sản Phẩm IT, Quản Lý Dự Án IT, Triển Khai Giải Pháp IT"
	wantOverview := Overview{
		Salary:           "20 - 26 triệu VNĐ",
		SalaryMin:        &minS,
		SalaryMax:        &maxS,
		SalaryCurrency:   "VND",
		SalaryUnit:       "MONTH",
		Location:         "Hà Nội",
		Experience:       "1 - 2 năm",
		ExperienceMonths: &months,
		Deadline:         "18/11/2026",
		ValidThrough:     "2026-11-18T00:00:00+0700",
		DatePosted:       "2026-10-19T10:38:19+0700",
		Level:            "Nhân Viên/Chuyên Viên",
		Education:        "Đại Học trở lên",
		Openings:         "5",
		WorkType:         "Toàn thời gian",
		EmploymentType:   "FULL_TIME",
		Industry:         industry,
		CompanyField:     "Công nghệ thông tin",
	}
	if !reflect.DeepEqual(d.Overview, wantOverview) {
		t.Errorf("overview\n got: %+v\nwant: %+v", d.Overview, wantOverview)
	}

	checks := []struct{ name, got, want string }{
		{"id", d.ID, "2161994"},
		{"title", d.Title, "Lập Trình Viên Full - Stack Java Developer (Onsite) - Thu Nhập Up To 26 Triệu - Nghỉ Thứ Bảy, Chủ Nhật"},
		{"company", d.Company, "Công ty Cổ phần EcoIT"},
		{"company_url", d.CompanyURL, "https://jobsgo.vn/tuyen-dung/cong-ty-co-phan-ecoit-41212265.html"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for name, s := range map[string]struct{ text, prefix, suffix string }{
		"description":  {d.Description, "- Phát triển hệ thống ebanking", "Làm việc tại văn phòng Lê Văn Lương, Hà Nội"},
		"requirements": {d.Requirements, "- Kiến thức:\n- Tốt nghiệp kỹ sư", "- Tinh thần trách nhiệm."},
		"benefits":     {d.Benefits, "- Thu nhập 20 - 26 triệu (tùy theo năng lực).", "theo chính sách của Công ty."},
	} {
		if !strings.HasPrefix(s.text, s.prefix) || !strings.HasSuffix(s.text, s.suffix) {
			t.Errorf("%s = %q", name, s.text)
		}
	}

	if !reflect.DeepEqual(d.WorkLocations, []string{"Lê Văn Lương, Hà Nội", "Hà Nội"}) {
		t.Errorf("work_locations = %v", d.WorkLocations)
	}
	if !reflect.DeepEqual(d.WorkingTimes, []string{"Từ Thứ 2 đến Thứ 6 (08:00 - 17:00)"}) {
		t.Errorf("working_times = %v", d.WorkingTimes)
	}
	wantInfo := map[string]string{
		"Yêu cầu ngôn ngữ": "Tiếng Anh",
		"Địa chỉ công ty":  "A10 – Ngõ 217 Đê La Thành – Đống Đa - Hà Nội",
	}
	if !reflect.DeepEqual(d.OtherInfo, wantInfo) {
		t.Errorf("other_info = %v", d.OtherInfo)
	}
}

// Without JSON-LD (some postings have none) the values shown on the page are kept.
func TestParseJobDetailWithoutJSONLD(t *testing.T) {
	page := strings.Replace(readFixture(t), `"@type":"JobPosting"`, `"@type":"Removed"`, 1)
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}
	if d.Overview.DatePosted != "19/10/2026" || d.Overview.Salary != "20 - 26 triệu VNĐ" || d.Overview.SalaryMin != nil {
		t.Errorf("overview = %+v", d.Overview)
	}
	if d.Company != "" || d.Description == "" {
		t.Errorf("company = %q, description empty = %v", d.Company, d.Description == "")
	}
}

func TestParseJobDetailEmptyPage(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body>nothing</body></html>")); err == nil {
		t.Fatal("expected error for a page without job content")
	}
}

func TestFetchJobDetails(t *testing.T) {
	fixture := readFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/viec-lam/a-1.html":
			w.Write([]byte(fixture))
		case "/viec-lam/b-2.html":
			// Rate limited once, then fine: the client must retry.
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Write([]byte(fixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	jobs := []Job{
		{ID: "1", URL: "https://jobsgo.vn/viec-lam/a-1.html?ref_component=job_list__item"},
		{ID: "2", URL: "b-2"}, // a bare slug goes through URL_DETAIL_JOB
		{ID: "3", URL: "https://jobsgo.vn/viec-lam/gone-3.html"},
	}
	var seen []string
	details, failures, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, func(d JobDetail) {
		seen = append(seen, d.ID)
	})
	if err != nil {
		t.Fatalf("FetchJobDetails: %v", err)
	}
	if len(details) != 2 || !reflect.DeepEqual(seen, []string{"1", "2"}) {
		t.Fatalf("got %d details, callback ids %v; want 2 and [1 2]", len(details), seen)
	}
	if details[0].URL != jobs[0].URL {
		t.Errorf("url = %q, want the search URL", details[0].URL)
	}
	if len(failures) != 1 || failures[0].ID != "3" || !strings.Contains(failures[0].Error, "not found") {
		t.Errorf("failures = %+v, want job 3 not found", failures)
	}
}

func TestFetchJobDetailsStopsWhenBlocked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	jobs := []Job{{ID: "1", URL: "a-1"}, {ID: "2", URL: "b-2"}}
	details, _, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 0 || hits.Load() != 1 {
		t.Errorf("got %d details after %d requests; want 0 details and a single request", len(details), hits.Load())
	}
}
