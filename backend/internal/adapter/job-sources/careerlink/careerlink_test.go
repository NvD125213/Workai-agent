package careerlink

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

// testdata/*.html are real CareerLink pages (12/11/2026), cut down to the parts the
// parsers read, with scripts (except the JSON-LD) and SVG paths removed.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLookupCity(t *testing.T) {
	for in, want := range map[string]string{
		"Hà Nội": "HN", "ha noi": "HN", "HN": "HN", "Thành phố Hà Nội": "HN",
		"Hồ Chí Minh": "HCM", "TP.HCM": "HCM", "Sài Gòn": "HCM",
		"Đà Nẵng": "DN", "Đồng Nai": "DNI", "Huế": "TTH", "Thừa Thiên Huế": "TTH",
		"Quảng Ninh": "QNH", "Quảng Nam": "QN", "Bà Rịa - Vũng Tàu": "BRVT", "Bắc Kạn": "BK",
		"dni": "DNI",
	} {
		if c, ok := LookupCity(in); !ok || c.Code != want {
			t.Errorf("LookupCity(%q) = %+v, %v; want %s", in, c, ok, want)
		}
	}
	if _, ok := LookupCity("Atlantis"); ok {
		t.Error("unknown city must not be found")
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL("Video  Editor", 1), "https://www.careerlink.vn/tim-viec-lam-tai/k/Video-Editor/ha-noi/HN"; got != want {
		t.Errorf("page 1 = %s, want %s", got, want)
	}
	if err := c.SetCity("TP.HCM"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.SearchURL("kế toán", 2), "https://www.careerlink.vn/tim-viec-lam-tai/k/k%E1%BA%BF-to%C3%A1n/ho-chi-minh/HCM?page=2"; got != want {
		t.Errorf("page 2 = %s, want %s", got, want)
	}
	if got, want := c.SearchURL("c#/.net", 1), "https://www.careerlink.vn/tim-viec-lam-tai/k/c%23%2F.net/ho-chi-minh/HCM"; got != want {
		t.Errorf("escaped = %s, want %s", got, want)
	}
	if err := c.SetCity(""); err != nil {
		t.Fatal(err)
	}
	if got, want := c.SearchURL("frontend developer", 3), "https://www.careerlink.vn/vieclam/list?keyword=frontend+developer&page=3"; got != want {
		t.Errorf("every city = %s, want %s", got, want)
	}
}

func TestParseSearchPage(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(readFixture(t, "search.html")))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalJobs != 222 || !res.hasNext || len(res.Jobs) != 4 {
		t.Fatalf("total %d, next %v, jobs %d", res.TotalJobs, res.hasNext, len(res.Jobs))
	}
	j := res.Jobs[1]
	if j.ID != "3676422" ||
		j.Title != "Nhân Viên Chăm Sóc Khách Hàng Ngân Hàng Quân Đội MB Bank - Không Yêu Cầu Kinh Nghiệm" ||
		j.URL != "https://www.careerlink.vn/tim-viec-lam/nhan-vien-cham-soc-khach-hang-ngan-hang-quan-doi-mb-bank-khong-yeu-cau-kinh-nghiem/3676422" ||
		j.Company != "Chi Nhánh Công Ty TNHH Transcosmos Việt Nam - Hồ Chí Minh" || j.Location != "Hà Nội" ||
		j.Salary != "7 triệu - 9 triệu" || j.CareerLevel != "Nhân viên" || j.UpdatedAt != "2026-11-12T02:29:29.000Z" ||
		!strings.HasPrefix(j.CompanyLogo, "https://blob-careerlinkvn.careerlink.vn/company_logos/") {
		t.Errorf("job 1 = %+v", j)
	}
	if j := res.Jobs[2]; j.Salary != "Thương lượng" {
		t.Errorf("job 2 = %+v", j)
	}
	if j := res.Jobs[3]; j.Location != "Hà Nội, Hồ Chí Minh, Đà Nẵng" || !strings.Contains(j.URL, "%E2%80%93") {
		t.Errorf("job 3 = %+v", j)
	}
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail.html")))
	if err != nil {
		t.Fatal(err)
	}
	want := Overview{
		Salary: "7 triệu - 9 triệu", Location: "Ô Chợ Dừa, Hà Nội", Experience: "0 - 1 năm kinh nghiệm",
		Education: "Trung cấp", WorkType: "Nhân viên toàn thời gian", CareerLevel: "Nhân viên",
		Industry:   "Thư ký / Hành chánh, Tư vấn dịch vụ khách hàng, Dịch vụ khách hàng",
		DatePosted: "2026-11-12", Deadline: "2026-12-12",
	}
	if d.Title != "Chuyên Viên Hỗ Trợ Khách Hàng - Trực Tổng Đài Ngân Hàng Quân Đội MB" ||
		d.Company != "Chi Nhánh Công Ty TNHH Transcosmos Việt Nam - Hồ Chí Minh" || d.Overview != want {
		t.Errorf("title %q, company %q, overview\n%+v\nwant\n%+v", d.Title, d.Company, d.Overview, want)
	}
	if !strings.HasPrefix(d.Description, "- Thực hiện giải đáp") || strings.Contains(d.Description, "Mô tả công việc") {
		t.Errorf("description:\n%s", d.Description)
	}
	if !strings.HasPrefix(d.Requirements, "- Tốt nghiệp Trung cấp") || !strings.Contains(d.Benefits, "Khám sức khỏe hàng năm") {
		t.Errorf("requirements:\n%s\nbenefits:\n%s", d.Requirements, d.Benefits)
	}
	if d.Info["Giới tính"] != "Bất kỳ" || len(d.Skills.All) != 10 || d.Skills.All[0] != "Nhân Viên Văn Phòng" {
		t.Errorf("info %v, tags %v", d.Info, d.Skills.All)
	}
	if d.Overview.Expired {
		t.Error("an open posting must not be expired")
	}
}

// The JSON-LD alone still gives a usable job.
func TestParseJobDetailJSONLDOnly(t *testing.T) {
	page := readFixture(t, "detail.html")
	page = page[:strings.Index(page, "<body>")] + "<body></body></html>"
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	o := d.Overview
	if d.Company == "" || !strings.Contains(d.Description, "Kinh nghiệm / Kỹ năng chi tiết") ||
		o.Location != "Hà Nội" || o.Experience != "Không yêu cầu kinh nghiệm" || o.WorkType != "Toàn thời gian" ||
		o.Deadline != "2026-12-12" {
		t.Errorf("detail = %+v", d)
	}
}

func TestParseJobDetailExpired(t *testing.T) {
	page := strings.Replace(readFixture(t, "detail.html"),
		`Hết hạn trong<!-- -->:<!-- --> <b class="">31 Ngày</b>`, `<b>Đã hết hạn</b>`, 1)
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil || !d.Overview.Expired {
		t.Errorf("expired %v, %v", d.Overview.Expired, err)
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	page := `<html><body><h1 class="mt-6">Rất tiếc, chúng tôi không tìm thấy việc làm bạn đang tìm kiếm</h1></body></html>`
	if _, err := ParseJobDetail(strings.NewReader(page)); !errors.Is(err, ErrNotFound) {
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
		if r.URL.Path != "/tim-viec-lam-tai/k/developer/ha-noi/HN" {
			t.Errorf("unexpected request %s", r.URL)
		}
		switch r.URL.Query().Get("page") {
		case "3": // last page: no next link
			w.Write([]byte(strings.Replace(page, `rel="next"`, `rel="nofollow"`, 1)))
			return
		case "99": // past the end: 200 without cards
			w.Write([]byte(`<html><body><p class="JobIndex-module__x__count"><strong>222</strong> việc làm</p><ul data-testid="job-results"></ul></body></html>`))
			return
		}
		w.Write([]byte(page))
	}))
	r1, err := c.FetchJobs(context.Background(), "developer", 1)
	if err != nil || r1.TotalPages != 5 { // ceil(222 / 50)
		t.Errorf("page 1: pages %d, %v", r1.TotalPages, err)
	}
	r3, err := c.FetchJobs(context.Background(), "developer", 3)
	if err != nil || r3.TotalPages != 3 {
		t.Errorf("page 3 without next link (and a short page): pages %d, %v", r3.TotalPages, err)
	}
	all, err := c.FetchAllJobs(context.Background(), "developer", 99)
	if err != nil || len(all) != 4 { // every page repeats the same 4 cards
		t.Errorf("all: %d jobs, %v", len(all), err)
	}
	r99, err := c.FetchJobs(context.Background(), "developer", 99)
	if err != nil || len(r99.Jobs) != 0 || r99.TotalPages > 99 {
		t.Errorf("page 99 must be an empty last page, got %+v, %v", r99, err)
	}
}

// Too many requests get the hCaptcha page, with status 200.
func TestCaptchaIsBlocked(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			// Body only, in case the header is ever dropped.
			w.Write([]byte(`<html><body><h1>Xác nhận bạn không phải robot</h1><div data-testid="captcha-widget"></div></body></html>`))
			return
		}
		w.Header().Set("X-Middleware-Rewrite", "/public/captcha?target_path=%2Ftim-viec-lam-tai")
		w.Write([]byte(`<html><body></body></html>`))
	}))
	for _, page := range []int{1, 2} {
		if _, err := c.FetchJobs(context.Background(), "developer", page); !errors.Is(err, ErrBlocked) {
			t.Errorf("page %d: err = %v, want ErrBlocked", page, err)
		}
	}
}

func TestFetchJobDetails(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/3676421"):
			w.Write([]byte(readFixture(t, "detail.html")))
		case strings.HasSuffix(r.URL.Path, "/1"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	jobs := []Job{
		{ID: "3676421", URL: "https://www.careerlink.vn/tim-viec-lam/chuyen-vien-ho-tro/3676421", Salary: "7 triệu - 9 triệu"},
		{ID: "1", URL: "https://www.careerlink.vn/tim-viec-lam/x-gone/1"},
		{ID: "2", URL: "https://www.careerlink.vn/tim-viec-lam/x-blocked/2"},
		{ID: "3", URL: "https://www.careerlink.vn/tim-viec-lam/x-never/3"},
	}
	details, failures, err := c.FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 1 || len(failures) != 1 || failures[0].ID != "1" {
		t.Fatalf("details %d, failures %+v", len(details), failures)
	}
	if d := details[0]; d.ID != "3676421" || d.URL != "https://www.careerlink.vn/tim-viec-lam/chuyen-vien-ho-tro/3676421" {
		t.Errorf("detail = %s %s", d.ID, d.URL)
	}
}
