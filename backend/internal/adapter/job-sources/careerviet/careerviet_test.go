package careerviet

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

// testdata/*.html are real CareerViet pages (12/11/2026), cut down to the parts the
// parsers read, with SVG icons removed.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL("Lập trình viên Golang", 1), "https://careerviet.vn/viec-lam/lap-trinh-vien-golang-tai-ha-noi-kl4-vi.html"; got != want {
		t.Errorf("page 1 = %s, want %s", got, want)
	}
	if got, want := c.SearchURL("developer", 2), "https://careerviet.vn/viec-lam/developer-tai-ha-noi-kl4-trang-2-vi.html"; got != want {
		t.Errorf("page 2 = %s, want %s", got, want)
	}
}

func TestParseSearchPage(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(readFixture(t, "search.html")))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalJobs != 437 || res.TotalPages != 9 || res.Page != 1 {
		t.Errorf("total %d, pages %d, page %d; want 437, 9 (50/page), 1", res.TotalJobs, res.TotalPages, res.Page)
	}
	if len(res.Jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(res.Jobs))
	}

	j := res.Jobs[0]
	if j.ID != "35C89EA2" || j.Title != "Business Development Manager" ||
		j.URL != "https://careerviet.vn/vi/tim-viec-lam/business-development-manager.35C89EA2.html" ||
		j.Company != "Công Ty Cổ Phần Năng Lượng Mặt Trời Nami" ||
		j.CompanyURL != "https://careerviet.vn/vi/nha-tuyen-dung/cong-ty-co-phan-nang-luong-mat-troi-nami.35A9C23A.html" ||
		j.Salary != "Cạnh tranh" || j.Location != "Hà Nội" ||
		j.Deadline != "2026-12-07" || j.UpdatedAt != "2026-11-12" {
		t.Errorf("job 0 = %+v", j)
	}
	if !strings.HasPrefix(j.CompanyLogo, "https://images.careerviet.vn/employer_folders/") {
		t.Errorf("logo = %q, want the image behind /_next/image", j.CompanyLogo)
	}
	if len(j.Industries) != 3 || j.Industries[0] != "Bán lẻ / Bán sỉ" {
		t.Errorf("industries = %v", j.Industries)
	}

	if got := salaryText(" Lương: 12 - 15 Triệu VND "); got != "12 - 15 Triệu VND" {
		t.Errorf("salaryText = %q, want the amount without the label", got)
	}

	// No date, only "Cơ hội ứng tuyển chỉ còn: 3 ngày".
	j = res.Jobs[2]
	if j.ID != "35C8AE46" || j.Deadline != "" || j.DeadlineIn != "3 ngày" || j.UpdatedAt != "2026-10-29" {
		t.Errorf("job 2 = %+v", j)
	}
}

func TestParseSearchPagePastTheEnd(t *testing.T) {
	page := `<html><body><h1>437 việc làm developer</h1><div class="pagination"><ul></ul></div></body></html>`
	res, err := ParseSearchPage(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Jobs) != 0 {
		t.Errorf("jobs = %d", len(res.Jobs))
	}
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail.html")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Software Engineer" || d.Company != "Công ty TNHH Viettel - CHT" ||
		d.CompanyURL != "https://careerviet.vn/vi/nha-tuyen-dung/cong-ty-tnhh-viettel-cht.35A87FE7.html" {
		t.Errorf("header = %q / %q / %q", d.Title, d.Company, d.CompanyURL)
	}
	o := d.Overview
	want := Overview{
		Salary: "Cạnh tranh", Location: "Hà Nội", Experience: "Trên 3 Năm", Level: "Nhân viên",
		WorkType: "Nhân viên chính thức", Industries: "CNTT - Phần mềm, CNTT - Phần cứng / Mạng",
		Education: "Đại học", UpdatedAt: "2026-11-04", Deadline: "2026-11-14",
		DatePosted: "2026-11-04T20:01:00.849Z", ValidThrough: "2026-11-15T23:59:00Z",
	}
	if o != want {
		t.Errorf("overview =\n%+v\nwant\n%+v", o, want)
	}
	for field, text := range map[string]string{"description": d.Description, "requirements": d.Requirements, "benefits": d.Benefits} {
		if text == "" {
			t.Errorf("%s is empty", field)
		}
	}
	if !strings.Contains(d.Description, "Trách nhiệm công việc chính") || strings.Contains(d.Description, "Mô tả Công việc") {
		t.Errorf("description (heading must be dropped):\n%s", d.Description)
	}
	if !strings.Contains(d.Requirements, "Golang, Python") {
		t.Errorf("requirements:\n%s", d.Requirements)
	}
	if !strings.HasPrefix(d.Benefits, "- Chế độ bảo hiểm\n") {
		t.Errorf("benefits:\n%s", d.Benefits)
	}
	if d.OtherInfo["Độ tuổi"] != "Không giới hạn tuổi" {
		t.Errorf("other info = %v", d.OtherInfo)
	}
	if len(d.WorkLocations) != 1 || !strings.Contains(d.WorkLocations[0], "Nguyễn Huy Tưởng") {
		t.Errorf("work locations = %v", d.WorkLocations)
	}
	if len(d.Skills.Keywords) == 0 || d.Skills.Keywords[len(d.Skills.Keywords)-1] != "php" {
		t.Errorf("keywords = %v", d.Skills.Keywords)
	}
}

// Some employers have their own page template: the summary is a table and the
// sections are outside .job-detail-content.
func TestParseJobDetailCustomTemplate(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "detail-custom.html")))
	if err != nil {
		t.Fatal(err)
	}
	o := d.Overview
	if d.Title != "Kỹ Sư Robotic" || o.Level != "Nhân viên" || o.WorkType != "Nhân viên chính thức" ||
		o.Deadline != "2026-12-02" || o.UpdatedAt != "2026-11-02" || o.Salary != "Cạnh tranh" ||
		o.Location != "Hưng Yên, Hà Nội" || o.Industries != "Cơ khí / Ô tô / Tự động hóa" {
		t.Errorf("title %q, overview %+v", d.Title, o)
	}
	if !strings.Contains(d.Description, "ROS/ROS2") || d.Requirements == "" || !strings.Contains(d.Benefits, "- Laptop") {
		t.Errorf("sections: description %d, requirements %d, benefits %q", len(d.Description), len(d.Requirements), d.Benefits)
	}
	if d.OtherInfo["Bằng cấp"] != "Đại học" || d.Overview.Education != "Đại học" {
		t.Errorf("other info = %v", d.OtherInfo)
	}
	for heading := range d.OtherSections {
		if strings.Contains(strings.ToLower(heading), "job tags") || heading == "Gợi ý hồ sơ" {
			t.Errorf("section %q must be skipped", heading)
		}
	}
}

// A page whose layout is unknown still gives the job from its JSON-LD.
func TestParseJobDetailFromJSONLD(t *testing.T) {
	page := `<html><head><script type="application/ld+json">{"@type":"JobPosting","title":"Video Editor",
		"description":"<p>Dựng video</p><ul><li>Premiere</li></ul>","validThrough":"2026-12-01T23:59:00Z",
		"experienceRequirements":{"description":"Kinh nghiệm 2 Năm"},
		"hiringOrganization":{"name":"ABC","url":"https://careerviet.vn/vi/nha-tuyen-dung/abc.1.html"},
		"jobLocation":{"address":{"addressLocality":"Hà Nội"}}}</script></head><body></body></html>`
	d, err := ParseJobDetail(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Video Editor" || d.Company != "ABC" || d.Overview.Experience != "2 Năm" ||
		d.Overview.Location != "Hà Nội" || !strings.Contains(d.Description, "Premiere") {
		t.Errorf("detail = %+v", d)
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body><h1>404</h1></body></html>")); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestIsoDate(t *testing.T) {
	for in, want := range map[string]string{
		"07-12-2026": "2026-12-07", "7/1/2026": "2026-01-07", "31-02-2026": "31-02-2026", "3 ngày": "3 ngày",
	} {
		if got := isoDate(in); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", in, got, want)
		}
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

func TestFetchJobsAndDetails(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/viec-lam/developer-tai-ha-noi-kl4-vi.html":
			w.Write([]byte(readFixture(t, "search.html")))
		case strings.HasSuffix(r.URL.Path, ".35C89EA2.html"):
			w.Write([]byte(readFixture(t, "detail.html")))
		case strings.HasSuffix(r.URL.Path, ".35C8B3BB.html"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, ".35C8AE46.html"):
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	ctx := context.Background()
	res, err := c.FetchJobs(ctx, "developer", 1)
	if err != nil {
		t.Fatal(err)
	}
	details, failures, err := c.FetchJobDetails(ctx, res.Jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked from the third job", err)
	}
	if len(details) != 1 || len(failures) != 1 || failures[0].ID != "35C8B3BB" {
		t.Fatalf("details %d, failures %+v", len(details), failures)
	}
	d := details[0]
	if d.ID != "35C89EA2" || d.URL != "https://careerviet.vn/vi/tim-viec-lam/business-development-manager.35C89EA2.html" {
		t.Errorf("detail id/url = %s %s", d.ID, d.URL)
	}
}
