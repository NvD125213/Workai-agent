package vieclam24h

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testdata/job-list.json is a real get-job-list answer (12/11/2026), cut to 2 jobs.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLookupProvince(t *testing.T) {
	for in, want := range map[string]int{
		"Hà Nội": 210000160, "ha noi": 210000160, "HN": 210000160, "Thành phố Hà Nội": 210000160,
		"TP.HCM": 210000187, "Hồ Chí Minh": 210000187, "Sài Gòn": 210000187,
		"Huế": 210000000, "Thừa Thiên Huế": 210000000, // checked below by name
		"Bắc Giang": 0, // merged into Bắc Ninh in 2025: the pre-merger entry
	} {
		p, ok := LookupProvince(in)
		if !ok {
			t.Errorf("LookupProvince(%q) not found", in)
			continue
		}
		switch in {
		case "Huế", "Thừa Thiên Huế":
			if p.Name != "Huế" || p.Version != 2 {
				t.Errorf("LookupProvince(%q) = %+v", in, p)
			}
		case "Bắc Giang":
			if p.Version != 1 || p.OldID == 0 {
				t.Errorf("LookupProvince(%q) = %+v, want the version 1 entry", in, p)
			}
		default:
			if p.ID != want {
				t.Errorf("LookupProvince(%q) = %+v, want id %d", in, p, want)
			}
		}
	}
	if _, ok := LookupProvince("Atlantis"); ok {
		t.Error("unknown province must not be found")
	}
	for _, id := range []int{210000160, 110000073, 73} {
		if p, ok := ProvinceByID(id); !ok || p.Name != "Hà Nội" {
			t.Errorf("ProvinceByID(%d) = %+v, %v", id, p, ok)
		}
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL(" video editor ", 2), "https://apiv2.vieclam24h.vn/employer/fe/job/get-job-list?page=2&per_page=50&provinces=210000160&q=video+editor"; got != want {
		t.Errorf("SearchURL =\n%s\nwant\n%s", got, want)
	}
	if err := c.SetProvince(""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.SearchURL("x", 1), "provinces=") {
		t.Error("every province must not send provinces=")
	}
	if err := c.SetProvince("Atlantis"); err == nil {
		t.Error("unknown province must fail")
	}
}

func TestParseJobList(t *testing.T) {
	res, err := ParseJobList(readFixture(t, "job-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalPages != 1 || len(res.Jobs) != 2 {
		t.Fatalf("pages %d, jobs %d", res.TotalPages, len(res.Jobs))
	}
	j := res.Jobs[0]
	want := Job{
		ID: "200975010", Source: Source, Title: "Java Backend Developer",
		URL:     "https://vieclam24h.vn/it-phan-mem/java-backend-developer-c8p210000160id200975010.html",
		Company: "CÔNG TY CỔ PHẦN CÔNG NGHỆ TASC VIỆT NAM", Salary: "10 - 17 triệu",
		SalaryMin: 10000000, SalaryMax: 17000000, Location: "Hà Nội",
		Experience: "2 năm", Level: "Nhân viên", WorkingMethod: "Toàn thời gian cố định",
		Gender: "Không yêu cầu", Vacancies: 3,
		PostedAt: "2026-10-26T21:34:58+07:00", Deadline: "2026-11-30T23:59:59+07:00",
	}
	if j.ID != want.ID || j.Title != want.Title || j.URL != want.URL || j.Company != want.Company ||
		j.Salary != want.Salary || j.SalaryMin != want.SalaryMin || j.SalaryMax != want.SalaryMax ||
		j.Location != want.Location || j.Experience != want.Experience || j.Level != want.Level ||
		j.WorkingMethod != want.WorkingMethod || j.Gender != want.Gender || j.Vacancies != want.Vacancies ||
		j.PostedAt != want.PostedAt || j.Deadline != want.Deadline {
		t.Errorf("job 0 =\n%+v\nwant\n%+v", j, want)
	}
	if !strings.Contains(j.Requirements, "Spring Boot") || strings.Contains(j.Requirements, "<") {
		t.Errorf("requirements (plain text):\n%.300s", j.Requirements)
	}
	if len(j.WorkLocations) != 1 || j.WorkLocations[0] != "87 - 89 Khuất Duy Tiến, Hà Nội" {
		t.Errorf("work locations = %v", j.WorkLocations)
	}
	if len(j.Occupations) == 0 || j.Occupations[0] != "IT Phần mềm" || len(j.Tags) == 0 {
		t.Errorf("occupations %v, tags %v", j.Occupations, j.Tags)
	}

	// Two provinces.
	if j := res.Jobs[1]; !strings.HasPrefix(j.Location, "Hà Nội, ") || strings.Count(j.Location, ",") != 1 {
		t.Errorf("job 1 location = %q", j.Location)
	}
}

// The API is loosely typed: approved_at can be false, numbers can be strings; an item
// that still cannot be read is skipped, not the whole page.
func TestParseJobListLooseTypes(t *testing.T) {
	body := `{"code":200,"msg":"ok","data":{"total_pages":"1","total_items":3,"items":[
		{"id":1,"title":"A","approved_at":false,"created_at":1793008157,"salary_min":"5000000","salary_max":null,"province_ids":["210000160"]},
		{"id":"2","title":"B","approved_at":1793025298},
		{"id":3,"title":"C","province_ids":"oops"}]}}`
	res, err := ParseJobList([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Jobs) != 2 || res.TotalPages != 1 {
		t.Fatalf("jobs %d, pages %d; want 2 (third skipped), 1", len(res.Jobs), res.TotalPages)
	}
	a := res.Jobs[0]
	if a.PostedAt != "2026-10-26T16:49:17+07:00" || a.Salary != "Từ 5 triệu" || a.Location != "Hà Nội" {
		t.Errorf("job A = %+v", a)
	}
	if res.Jobs[1].ID != "2" {
		t.Errorf("job B id = %q", res.Jobs[1].ID)
	}
}

func TestParseJobListAPIError(t *testing.T) {
	_, err := ParseJobList([]byte(`{"code":0,"msg":"Đã có lỗi xảy ra, vui lòng thử lại sau.","data":[]}`))
	if err == nil || !strings.Contains(err.Error(), "Đã có lỗi") {
		t.Errorf("err = %v", err)
	}
}

func TestSalaryText(t *testing.T) {
	for _, c := range []struct {
		min, max int64
		want     string
	}{
		{10000000, 17000000, "10 - 17 triệu"},
		{7500000, 9000000, "7.5 - 9 triệu"},
		{12000000, 0, "Từ 12 triệu"},
		{0, 30000000, "Tới 30 triệu"},
		{15000000, 15000000, "15 triệu"},
		{0, 0, "Thỏa thuận"},
	} {
		if got := salaryText(c.min, c.max); got != c.want {
			t.Errorf("salaryText(%d, %d) = %q, want %q", c.min, c.max, got, c.want)
		}
	}
}

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client(), srv.URL)
	c.siteBase = srv.URL // token lookups must never reach the real site from a test
	c.SetRequestDelay(0, 0)
	c.retryBase = time.Millisecond
	return c
}

func TestFetchJobsSendsTokenAndRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+GuestToken || r.Header.Get("Origin") != "https://vieclam24h.vn" {
			t.Errorf("headers = %v", r.Header)
		}
		if r.URL.Path != searchPath || r.URL.Query().Get("provinces") != "210000160" {
			t.Errorf("url = %s", r.URL)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(readFixture(t, "job-list.json"))
	}))
	jobs, err := c.FetchAllJobs(context.Background(), "developer", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || calls.Load() != 2 {
		t.Errorf("jobs %d, calls %d (one 429 retried, then total_pages=1 stops)", len(jobs), calls.Load())
	}
}
