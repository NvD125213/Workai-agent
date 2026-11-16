package topcv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const sampleJobItem = `
<div class="job-item-search-result bg-yellow job-ta" data-job-id="%s" data-job-position="1">
  <div class="avatar">
    <a href="https://www.topcv.vn/viec-lam/backend/%[1]s.html?ta_source=X&amp;u_sr_id=Y">
      <img data-src="https://cdn.topcv.vn/logo.jpg" class="w-100 lazy">
    </a>
  </div>
  <div class="body">
    <h3 class="title">
      <a target="_blank" href="https://www.topcv.vn/viec-lam/backend/%[1]s.html?ta_source=X&amp;u_sr_id=Y">
        <span title="Lập Trình Viên Backend (Java/Golang)">Lập Trình Viên Backend (Java/Golang)</span>
      </a>
    </h3>
    <a class="company" href="https://www.topcv.vn/cong-ty/aiontech/75602.html">
      <span class="company-name" title="CÔNG TY AIONTECH">CÔNG TY AIONTECH</span>
    </a>
    <div class="info">
      <label class="salary"><span>
          Thoả thuận
      </span></label>
      <a class="address"><span class="city-text">
          Hà Nội
      </span></a>
      <label class="exp"><span> 2 năm </span></label>
    </div>
    <div class="box-icon">
      <div class="tag">
        <span class="item-tag">2 năm kinh nghiệm chuyên môn</span><span class="item-tag">Backend Deve...</span><span title="Backend Developer, IT - Phần mềm" class="remaining-items">+2</span>
      </div>
      <label class="address label-update" title="Cập nhật 11 phút trước">
        <span class="hidden-on-quick-view">Đăng </span>
        1 tuần trước
      </label>
    </div>
  </div>
</div>`

const samplePagination = `
<nav class="box-pagination"><ul class="pagination"><li>
  <span id="job-listing-paginate-text"><span class="hight-light">%d&nbsp;</span>/&nbsp;%d trang</span>
</li></ul></nav>`

func samplePage(page, total int, ids ...string) string {
	var b strings.Builder
	b.WriteString("<html><body><div class=\"job-list-search-result\">")
	for _, id := range ids {
		fmt.Fprintf(&b, sampleJobItem, id)
	}
	b.WriteString("</div>")
	if total > 0 {
		fmt.Fprintf(&b, samplePagination, page, total)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestParseSearchPage(t *testing.T) {
	result, err := ParseSearchPage(strings.NewReader(samplePage(2, 3, "2320491", "2320491")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if result.Page != 2 || result.TotalPages != 3 {
		t.Errorf("page = %d/%d, want 2/3", result.Page, result.TotalPages)
	}
	if len(result.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1 (duplicates removed)", len(result.Jobs))
	}

	job := result.Jobs[0]
	job.ScrapedAt = job.ScrapedAt.Truncate(0)
	want := Job{
		ID:          "2320491",
		Source:      Source,
		Title:       "Lập Trình Viên Backend (Java/Golang)",
		URL:         "https://www.topcv.vn/viec-lam/backend/2320491.html",
		Company:     "CÔNG TY AIONTECH",
		CompanyURL:  "https://www.topcv.vn/cong-ty/aiontech/75602.html",
		CompanyLogo: "https://cdn.topcv.vn/logo.jpg",
		Salary:      "Thoả thuận",
		Location:    "Hà Nội",
		Experience:  "2 năm",
		Tags:        []string{"2 năm kinh nghiệm chuyên môn", "Backend Developer", "IT - Phần mềm"},
		PostedAt:    "1 tuần trước",
		UpdatedAt:   "Cập nhật 11 phút trước",
		ScrapedAt:   job.ScrapedAt,
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job mismatch\n got: %+v\nwant: %+v", job, want)
	}
}

func TestParseSearchPageWithoutPagination(t *testing.T) {
	result, err := ParseSearchPage(strings.NewReader(samplePage(0, 0, "1", "2")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if len(result.Jobs) != 2 || result.Page != 1 || result.TotalPages != 1 {
		t.Errorf("got %d jobs, page %d/%d; want 2 jobs, page 1/1", len(result.Jobs), result.Page, result.TotalPages)
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"golang":                "golang",
		"Lập trình viên Golang": "lap-trinh-vien-golang",
		"  Kỹ sư Đảm bảo  chất lượng ": "ky-su-dam-bao-chat-luong",
		"C#/.NET Developer":            "c-net-developer",
	}
	for in, want := range tests {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "http://localhost:1234/", "")
	got := c.SearchURL("Lập trình viên", 2)
	want := "http://localhost:1234/tim-viec-lam-lap-trinh-vien-tai-ha-noi-kl1?type_keyword=1&disable_auto_detect_type_keyword=1&page=2&locations=l1&saturday_status=0&sba=1"
	if got != want {
		t.Errorf("SearchURL = %q, want %q", got, want)
	}
}

func TestFetchAllJobs(t *testing.T) {
	pages := map[string]string{
		"1": samplePage(1, 2, "1", "2"),
		"2": samplePage(2, 2, "2", "3"),
	}
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/tim-viec-lam-golang-tai-ha-noi-kl1") {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		requested = append(requested, page)
		fmt.Fprint(w, pages[page])
	}))
	defer srv.Close()

	jobs, err := newTestClient(srv).FetchAllJobs(context.Background(), "golang", 5)
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
	if !reflect.DeepEqual(requested, []string{"1", "2"}) {
		t.Errorf("requested pages = %v, want [1 2]", requested)
	}
}

func TestFetchJobsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).FetchJobs(context.Background(), "golang", 1); err == nil {
		t.Fatal("expected error on 403")
	}
}
