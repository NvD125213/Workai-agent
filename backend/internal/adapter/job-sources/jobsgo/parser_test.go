package jobsgo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// newTestClient talks to srv without any politeness delays.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.Client(), srv.URL)
	c.SetRequestDelay(0, 0)
	c.retryBase = 0
	c.batchPause = 0
	return c
}

// sampleJobCard is a job card from https://jobsgo.vn/viec-lam-java-tai-ha-noi.html.
const sampleJobCard = `
<div class="col-grid">
<div  data-id="%[1]s" class="card job-card rounded-3 h-100 p-3">
    <div class="text-decoration-none text-dark d-block h-100  ">
        <div class="d-flex">
            <a href="https://jobsgo.vn/viec-lam/lap-trinh-vien-java-%[1]s.html?ref_component=job_list__item" class="image-wrapper" title="Lập Trình Viên Java">
                <img src="https://media.jobsgo.vn/media/img/employer/567-200x200.jpg?v=1595581475" alt="Công ty Cổ phần EcoIT" class="rounded me-3 flex-shrink-0" width="80" height="80" loading="lazy">
            </a>
            <div class="flex-grow-1">
                <h3 class="job-title fw-bold mb-1"><a href="https://jobsgo.vn/viec-lam/lap-trinh-vien-java-%[1]s.html?ref_component=job_list__item" title="Lập Trình Viên Full - Stack Java Developer (Onsite)" class="text-decoration-none text-dark d-block h-100  " target="_blank"> Lập Trình Viên Full - Stack Java Developer (Onsite)</a></h3>
                <a href="https://jobsgo.vn/tuyen-dung/cong-ty-co-phan-ecoit-41212265.html" class="text-muted small company-title" title="Công ty Cổ phần EcoIT" target="_blank">
                    Công ty Cổ phần EcoIT                        </a>
                <div class="mt-1 text-primary fw-semibold small d-flex flex-wrap align-items-center">
                    <span>20 - 26 triệu VNĐ</span>
                    <span class="px-2 text-light pb-1">|</span>
                    <span>Hà Nội, </span>
                </div>
            </div>
        </div>
    </div>
    <div class="d-flex justify-content-between align-items-center mt-2">
        <div class="d-flex flex-wrap gap-1 small">
            <span title="Loại hình" class="badge bg-light text-secondary fw-normal badge-custom text-nowrap">
                Full-time                    </span>
            <span title="Yêu cầu kinh nghiệm" class="badge bg-light text-secondary fw-normal badge-custom text-nowrap">
                1 - 2 năm                    </span>
            <span title="Thời gian cập nhật" class="badge bg-light text-secondary fw-normal badge-custom text-nowrap">
                1 giờ trước                    </span>
        </div>
    </div>
</div>
</div>`

func samplePage(total, page int, ids ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<html><body><h1>Tuyển dụng %d việc làm Java tại Hà Nội  - 20/10/2026</h1>`, total)
	for _, id := range ids {
		fmt.Fprintf(&b, sampleJobCard, id)
	}
	// A suggestion outside the result list: a card without a job title must be skipped.
	b.WriteString(`<div data-id="999" class="card job-card"><h2 class="section-title">Việc Làm Cùng Công Ty</h2></div>`)
	if page > 0 {
		b.WriteString(`<ul class="pagination">`)
		for p := 1; p <= 2; p++ {
			class := ""
			if p == page {
				class = ` class="active"`
			}
			fmt.Fprintf(&b, `<li%s><a href="/viec-lam-java-tai-ha-noi.html?page=%d" data-page="%d">%[2]d</a></li>`, class, p, p-1)
		}
		b.WriteString(`<li class="next"><a href="/viec-lam-java-tai-ha-noi.html?page=2">»</a></li></ul>`)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestParseSearchPage(t *testing.T) {
	result, err := ParseSearchPage(strings.NewReader(samplePage(1046, 1, "2161994", "2161994")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	// The pager shows 2 pages, but 1046 jobs at 50 per page make 21.
	if result.Page != 1 || result.TotalPages != 21 || result.TotalJobs != 1046 {
		t.Errorf("page = %d/%d, total %d; want 1/21, 1046", result.Page, result.TotalPages, result.TotalJobs)
	}
	if len(result.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1 (duplicates and suggestions removed)", len(result.Jobs))
	}

	job := result.Jobs[0]
	want := Job{
		ID:          "2161994",
		Source:      Source,
		Title:       "Lập Trình Viên Full - Stack Java Developer (Onsite)",
		URL:         "https://jobsgo.vn/viec-lam/lap-trinh-vien-java-2161994.html",
		Company:     "Công ty Cổ phần EcoIT",
		CompanyURL:  "https://jobsgo.vn/tuyen-dung/cong-ty-co-phan-ecoit-41212265.html",
		CompanyLogo: "https://media.jobsgo.vn/media/img/employer/567-200x200.jpg?v=1595581475",
		Salary:      "20 - 26 triệu VNĐ",
		Location:    "Hà Nội",
		Experience:  "1 - 2 năm",
		WorkType:    "Full-time",
		UpdatedAt:   "1 giờ trước",
		ScrapedAt:   job.ScrapedAt,
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job mismatch\n got: %+v\nwant: %+v", job, want)
	}
}

func TestParseSearchPageWithoutPagination(t *testing.T) {
	result, err := ParseSearchPage(strings.NewReader(samplePage(2, 0, "1", "2")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if len(result.Jobs) != 2 || result.TotalPages != 1 {
		t.Errorf("got %d jobs, %d pages; want 2 jobs, 1 page", len(result.Jobs), result.TotalPages)
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "http://localhost:1234/")
	if got, want := c.SearchURL("Lập trình viên Java", 1), "http://localhost:1234/viec-lam-lap-trinh-vien-java-tai-ha-noi.html"; got != want {
		t.Errorf("SearchURL page 1 = %q, want %q", got, want)
	}
	if got, want := c.SearchURL("java", 3), "http://localhost:1234/viec-lam-java-tai-ha-noi.html?page=3"; got != want {
		t.Errorf("SearchURL page 3 = %q, want %q", got, want)
	}
}

func TestFetchAllJobs(t *testing.T) {
	pages := map[string]string{
		"":  samplePage(60, 1, "1", "2"),
		"2": samplePage(60, 2, "2", "3"),
	}
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/viec-lam-java-tai-ha-noi.html" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		requested = append(requested, page)
		fmt.Fprint(w, pages[page])
	}))
	defer srv.Close()

	jobs, err := newTestClient(srv).FetchAllJobs(context.Background(), "java", 5)
	if err != nil {
		t.Fatalf("FetchAllJobs: %v", err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	if !reflect.DeepEqual(ids, []string{"1", "2", "3"}) {
		t.Errorf("ids = %v, want [1 2 3]", ids)
	}
	if !reflect.DeepEqual(requested, []string{"", "2"}) {
		t.Errorf("requested pages = %q, want [\"\" \"2\"]", requested)
	}
}

func TestFetchJobsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).FetchJobs(context.Background(), "java", 1); err == nil {
		t.Fatal("expected error on 400")
	}
}
