package itviec

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

// testdata/job-content.html is a real response of
// https://itviec.com/it-jobs/principal-golang-engineer-in-hcm-hanoi-money-forward-vietnam-co-ltd-2356/content?locale=vi
func readFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/job-content.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const notFoundPage = `<!DOCTYPE html><html><head><title>Oops! This page has found a better job</title></head><body><h1>Oops!</h1></body></html>`

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t)))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}

	wantOverview := Overview{
		Salary:      "",
		WorkType:    "Linh hoạt (Tại văn phòng hoặc làm từ xa)",
		DatePosted:  "6 ngày trước",
		JobFunction: "Lập trình viên Backend",
		Domains:     []string{"Thương Mại Điện Tử", "Ngân Hàng", "Thuê Ngoài Phát Triển Phần Mềm", "Sản Phẩm Phần Mềm và Dịch Vụ Web", "Dịch Vụ Tài Chính"},
	}
	if !reflect.DeepEqual(d.Overview, wantOverview) {
		t.Errorf("overview\n got: %+v\nwant: %+v", d.Overview, wantOverview)
	}

	checks := []struct{ name, got, want string }{
		{"title", d.Title, "Principal Golang Engineer in HCM/Hanoi"},
		{"url", d.URL, "https://itviec.com/viec-lam-it/principal-golang-engineer-in-hcm-hanoi-money-forward-vietnam-co-ltd-2356"},
		{"company", d.Company, "MONEY FORWARD VIETNAM CO.,LTD"},
		{"company_url", d.CompanyURL, "https://itviec.com/nha-tuyen-dung/money-forward-vietnam-co-ltd"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for name, s := range map[string]struct{ text, prefix, contains string }{
		"description":  {d.Description, "We are seeking an experienced Principal/Senior Golang Engineer", "\n- Lead the design and development of complex software systems using Golang"},
		"requirements": {d.Requirements, "Qualifications:\n- 8+ years of software engineering experience", "Preferred Skills:"},
		"benefits":     {d.Benefits, "Caring Mental & Physical Recreation:\n- Hybrid working", "- Performance review twice/year"},
	} {
		if !strings.HasPrefix(s.text, s.prefix) || !strings.Contains(s.text, s.contains) {
			t.Errorf("%s = %q", name, s.text)
		}
	}

	if !reflect.DeepEqual(d.Skills.All, []string{"Golang", "System Architecture", "Design Systems", "LLM", "AI", "English"}) {
		t.Errorf("skills = %v", d.Skills.All)
	}
	if !reflect.DeepEqual(d.WorkLocations, []string{"Tầng 11, ROX Tower, 54A Nguyễn Chí Thanh,, Láng, Hà Nội"}) {
		t.Errorf("work_locations = %v", d.WorkLocations)
	}
	if !reflect.DeepEqual(d.TopReasons, []string{"Hybrid working; Flexible time; Macbook Pro", "L&D programs; 17 days off", "13.7 million B2C users; 140,000+ business customer"}) {
		t.Errorf("top_reasons = %v", d.TopReasons)
	}
	wantInfo := map[string]string{
		"Mô hình công ty":    "Sản phẩm",
		"Lĩnh vực công ty":   "Dịch Vụ Tài Chính",
		"Quy mô công ty":     "301-500",
		"Quốc gia":           "Japan",
		"Thời gian làm việc": "Thứ 2 - Thứ 6",
		"Làm việc ngoài giờ": "Không có OT",
	}
	if !reflect.DeepEqual(d.CompanyInfo, wantInfo) {
		t.Errorf("company_info = %v", d.CompanyInfo)
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader(notFoundPage)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchJobDetails(t *testing.T) {
	fixture := readFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("locale") != "vi" {
			http.Error(w, "missing locale", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/it-jobs/a-1/content":
			w.Write([]byte(fixture))
		case "/it-jobs/b-2/content":
			// Rate limited once, then fine: the client must retry.
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Write([]byte(fixture))
		default:
			w.Write([]byte(notFoundPage))
		}
	}))
	defer srv.Close()

	jobs := []Job{
		{ID: "a-1", URL: "https://itviec.com/viec-lam-it/a-1", Location: "Hà Nội", Expertise: "ignored, the page has one"},
		{ID: "b-2", URL: "https://itviec.com/viec-lam-it/b-2", Location: "Hà Nội"},
		{ID: "gone-3", URL: "https://itviec.com/viec-lam-it/gone-3"},
	}
	var seen []string
	details, failures, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, func(d JobDetail) {
		seen = append(seen, d.ID)
	})
	if err != nil {
		t.Fatalf("FetchJobDetails: %v", err)
	}
	if len(details) != 2 || !reflect.DeepEqual(seen, []string{"a-1", "b-2"}) {
		t.Fatalf("got %d details, callback ids %v; want 2 and [a-1 b-2]", len(details), seen)
	}
	d := details[0]
	if d.URL != jobs[0].URL || d.Overview.Location != "Hà Nội" || d.Overview.JobFunction != "Lập trình viên Backend" {
		t.Errorf("search data not merged: url=%q location=%q job_function=%q", d.URL, d.Overview.Location, d.Overview.JobFunction)
	}
	if len(failures) != 1 || failures[0].ID != "gone-3" || !strings.Contains(failures[0].Error, "not found") {
		t.Errorf("failures = %+v, want gone-3 not found", failures)
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

	jobs := []Job{{ID: "a-1"}, {ID: "b-2"}}
	details, _, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 0 || hits.Load() != 1 {
		t.Errorf("got %d details after %d requests; want 0 details and a single request", len(details), hits.Load())
	}
}
