package indeed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// testdata/job-detail.html is https://vn.indeed.com/jobs?q=&l=Hà+Nội&vjk=631bc7a7fec771a3
// (a job relayed from JobsGo, with "<b>Heading:</b>" paragraphs) and job-detail-inline.html
// the side pane of b13e17ce32ad3f3b (relayed from Viecoi, bare-text headings), both reduced
// to the window._initialData fields the adapter reads.

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFile(t, "job-detail.html")))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}

	salary := 20000000.0
	wantOverview := Overview{
		Salary:         "20.000.000 đ / năm",
		SalaryMin:      &salary,
		SalaryMax:      &salary,
		SalaryCurrency: "VND",
		SalaryUnit:     "YEARLY",
		Location:       "Hà Nội",
		Age:            "Vừa đăng",
		OriginSource:   "JobsGo",
	}
	if !reflect.DeepEqual(d.Overview, wantOverview) {
		t.Errorf("overview\n got: %+v\nwant: %+v", d.Overview, wantOverview)
	}

	checks := []struct{ name, got, want string }{
		{"id", d.ID, "631bc7a7fec771a3"},
		{"url", d.URL, "https://vn.indeed.com/viewjob?jk=631bc7a7fec771a3"},
		{"title", d.Title, "Kế Toán Tổng Hợp - Lương Cứng Upto 20 Triệu / Tháng"},
		{"company", d.Company, "Công Ty Cổ Phần Fsi Việt Nam"},
		{"company_url", d.CompanyURL, "https://vn.indeed.com/cmp/C%C3%B4ng-Ty-C%E1%BB%95-Ph%E1%BA%A7n-Fsi-Vi%E1%BB%87t-Nam"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for name, s := range map[string]struct{ text, prefix, suffix string }{
		"description":  {d.Description, "(Mức lương: Đến 20 triệu VNĐ)\n- Kiểm soát hồ sơ", "- Hỗ trợ công tác chấm công, tính lương, BHXH."},
		"requirements": {d.Requirements, "- Tốt nghiệp từ cao đẳng/đại học", "3 - 4 năm"},
		"benefits":     {d.Benefits, "- Lương cứng: 15.000.000 - 18.000.000", "khám sức khỏe định kỳ."},
	} {
		if !strings.HasPrefix(s.text, s.prefix) || !strings.HasSuffix(s.text, s.suffix) {
			t.Errorf("%s = %q", name, s.text)
		}
	}
	if d.OtherInfo["Chức vụ"] != "Nhân Viên/Chuyên Viên" || d.OtherInfo["Yêu cầu bằng cấp (tối thiểu)"] != "Cao Đẳng" {
		t.Errorf("other_info = %v", d.OtherInfo)
	}
	if len(d.OtherSections) != 0 {
		t.Errorf("other_sections = %v, want none", d.OtherSections)
	}
}

func TestParseJobDetailInlineHeadings(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFile(t, "job-detail-inline.html")))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}
	for name, s := range map[string]struct{ text, prefix, suffix string }{
		"description":  {d.Description, "- Be involved in Web", "Làm việc trực tiếp với PM"},
		"requirements": {d.Requirements, "- Bachelor degree", "- Able to quickly learn new technologies"},
		"benefits":     {d.Benefits, "- Salary review twice a year.", "awareness of advanced technologies."},
	} {
		if !strings.HasPrefix(s.text, s.prefix) || !strings.HasSuffix(s.text, s.suffix) {
			t.Errorf("%s = %q", name, s.text)
		}
	}
	if got := d.OtherSections["Salary"]; !strings.HasPrefix(got, "- Senior Golang dev (internal): Up to 2000$") {
		t.Errorf("other_sections[Salary] = %q", got)
	}
	if d.OtherInfo["Hạn nộp"] != "2026-10-28" || d.OtherInfo["Kỹ năng"] != "Golang, Javascript, HTML, CSS, Tiếng Anh" {
		t.Errorf("other_info = %v", d.OtherInfo)
	}
	if d.Overview.Salary != "" || d.Overview.OriginSource != "Viecoi" {
		t.Errorf("overview = %+v", d.Overview)
	}
}

func TestParseDescriptionWithoutHeadings(t *testing.T) {
	var d JobDetail
	parseDescription("<p>Phát triển backend bằng Go.</p><ul><li>Làm việc với Kafka</li></ul>", &d)
	if d.Description != "Phát triển backend bằng Go.\n- Làm việc với Kafka" || d.Requirements != "" {
		t.Errorf("description = %q, requirements = %q", d.Description, d.Requirements)
	}
}

func TestParseJobDetailEmptyPane(t *testing.T) {
	// Expired or unknown job keys get the search page without a side pane.
	page := `<script>window._initialData={"totalJobCount":3,"autoOpenTwoPaneViewjobResponse":null};</script>`
	if _, err := ParseJobDetail(strings.NewReader(page)); !errors.Is(err, errNoJobContent) {
		t.Fatalf("err = %v, want errNoJobContent", err)
	}
	if _, err := ParseJobDetail(strings.NewReader("<html><body>nothing</body></html>")); err == nil {
		t.Fatal("expected error for a page without job data")
	}
}

func TestJobKeyOf(t *testing.T) {
	for in, want := range map[string]string{
		"631bc7a7fec771a3": "631bc7a7fec771a3",
		"https://vn.indeed.com/viewjob?jk=631bc7a7fec771a3":                              "631bc7a7fec771a3",
		"https://vn.indeed.com/jobs?q=golang&l=H%C3%A0+N%E1%BB%99i&vjk=b5bdabe5b8c19df1": "b5bdabe5b8c19df1",
		"/rc/clk?jk=b13e17ce32ad3f3b&bb=abc":                                             "b13e17ce32ad3f3b",
		"https://vn.indeed.com/cmp/Dnse":                                                 "",
	} {
		if got := jobKeyOf(in); got != want {
			t.Errorf("jobKeyOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchJobDetails(t *testing.T) {
	fixture := readFile(t, "job-detail.html")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs" || r.URL.Query().Get("l") != "Hà Nội" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("vjk") {
		case "631bc7a7fec771a3":
			// Rate limited once, then fine: the client must retry.
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Write([]byte(fixture))
		default:
			// Indeed shows the plain search page when the job is gone.
			w.Write([]byte(`<script>window._initialData={"autoOpenTwoPaneViewjobResponse":null};</script>`))
		}
	}))
	defer srv.Close()

	jobs := []Job{
		{ID: "631bc7a7fec771a3", URL: ViewJobURL("631bc7a7fec771a3"), WorkType: "Toàn thời gian"},
		{ID: "0000000000000000", URL: ViewJobURL("0000000000000000")},
	}
	var seen []string
	details, failures, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, func(d JobDetail) {
		seen = append(seen, d.ID)
	})
	if err != nil {
		t.Fatalf("FetchJobDetails: %v", err)
	}
	if len(details) != 1 || !reflect.DeepEqual(seen, []string{"631bc7a7fec771a3"}) {
		t.Fatalf("got %d details, callback ids %v; want 1 and [631bc7a7fec771a3]", len(details), seen)
	}
	if details[0].Overview.WorkType != "Toàn thời gian" {
		t.Errorf("work_type = %q, want it merged from the search job", details[0].Overview.WorkType)
	}
	if len(failures) != 1 || failures[0].ID != "0000000000000000" || !strings.Contains(failures[0].Error, "not found") {
		t.Errorf("failures = %+v, want job 0000000000000000 not found", failures)
	}
}

func TestFetchJobDetailWrongPane(t *testing.T) {
	// If Indeed opens another job in the pane, the requested one is treated as gone.
	fixture := readFile(t, "job-detail.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).FetchJobDetail(context.Background(), "b13e17ce32ad3f3b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
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

	jobs := []Job{{ID: "631bc7a7fec771a3"}, {ID: "b13e17ce32ad3f3b"}}
	details, _, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 0 || hits.Load() != 1 {
		t.Errorf("got %d details after %d requests; want 0 details and a single request", len(details), hits.Load())
	}
}
