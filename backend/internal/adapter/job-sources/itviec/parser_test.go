package itviec

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

// sampleJobCard is a trimmed job card from https://itviec.com/it-jobs/golang/ha-noi?locale=vi.
const sampleJobCard = `
<div class='job-card ipt-2 d-flex flex-column super-hot' data-controller='search--job-selection' data-job-key='8a6e4009' data-search--job-selection-job-slug-value='%[1]s' data-search--job-selection-job-url-value='/it-jobs/%[1]s/content?job_index=0&amp;locale=vi'>
<div class='ipx-4 ipx-xl-3'>
<div class='ipy-2'>
<div class='d-flex align-items-center justify-content-between position-relative'>
<span class='small-text text-dark-grey'>
Đăng
6 ngày trước
</span>
</div>
<h3 class='imt-3 text-break' data-controller='utm-tracking'>
<a target="_blank" class="text-it-black text-hover-red" href="https://itviec.com/viec-lam-it/%[1]s?lab_feature=preview_jd_page">Principal Golang Engineer in HCM/Hanoi</a>
</h3>
<div class='imy-3 d-flex align-items-center'>
<a target="_blank" title="Top 1 Fintech Company" class="bg-white logo-employer-card" href="/nha-tuyen-dung/money-forward?lab_feature=preview_jd_page"><picture>
<img alt='Logo' data-controller='lazyload' data-src='https://itviec.com/logo.png' height='48' width='48'>
</picture>
</a><span class='ims-2 small-text text-hover-underline'>
<a target="_blank" class="text-rich-grey" href="/nha-tuyen-dung/money-forward?lab_feature=preview_jd_page">MONEY FORWARD VIETNAM CO.,LTD</a>
</span>
</div>
<div class='ipb-3 fw-500'>
<div class='d-flex align-items-center salary text-rich-grey'>
<svg></svg>
<a class="sign-in-view-salary text-reset ips-2" href="/sign_in?job=%[1]s">Đăng nhập để xem mức lương</a>
</div>
</div>
<div class='imt-1 d-flex align-items-center'>
<svg class="feather-icon icon-sm text-dark-grey"></svg>
<a class="position-relative stretched-link text-rich-grey" title="Backend Developer" href="/viec-lam-it/backend-developer">Backend Developer</a>
</div>
<div class='imt-1 d-flex align-items-center text-dark-grey igap-2 small-text'>
<div class='flex-shrink-0 ipb-1'><svg></svg></div>
<div class='text-rich-grey flex-shrink-0'>
Linh hoạt
</div>
<div class='dot-icon flex-shrink-0'></div>
<div class='text-rich-grey text-truncate text-nowrap stretched-link position-relative' title='Hà Nội'>
Hà Nội
</div>
</div>
<div class='imt-4 imb-3 d-flex igap-1 d-none' data-controller='responsive-tag-list'>
<a class="text-reset text-nowrap stretched-link itag itag-light itag-sm" href="/viec-lam-it/golang?click_source=Skill+tag">Golang</a>
<a class="text-reset text-nowrap stretched-link itag itag-light itag-sm" href="/viec-lam-it/ai?click_source=Skill+tag">AI</a>
<a class="text-reset text-nowrap stretched-link itag itag-light itag-sm" href="/viec-lam-it/golang?click_source=Skill+tag">Golang</a>
</div>
<div class='small-text text-it-black fw-500'>
<ul>
<li class='imb-1'>Hybrid working; Flexible time; Macbook Pro</li>
<li class='imb-1'>L&amp;D programs; 17 days off </li>
</ul>
</div>
</div>
</div>
</div>`

func samplePage(total, page, pages int, slugs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<html><body><h1 class='headline-total-jobs search-header'>%d <span class="text-it-red">golang</span> việc làm tại Hà Nội</h1>`, total)
	for _, slug := range slugs {
		fmt.Fprintf(&b, sampleJobCard, slug)
	}
	if pages > 0 {
		b.WriteString(`<nav class='ipagination imt-10'>`)
		for p := 1; p <= pages; p++ {
			if p == page {
				fmt.Fprintf(&b, `<div class='page current'> %d </div>`, p)
			} else {
				fmt.Fprintf(&b, `<div class='page'><a href="/it-jobs/golang/ha-noi?page=%d">%[1]d</a></div>`, p)
			}
		}
		b.WriteString(`<div class='page next'><a rel="next" href="#"><svg></svg></a></div></nav>`)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestParseSearchPage(t *testing.T) {
	slug := "principal-golang-engineer-in-hcm-hanoi-money-forward-vietnam-co-ltd-2356"
	result, err := ParseSearchPage(strings.NewReader(samplePage(47, 2, 3, slug, slug)))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if result.Page != 2 || result.TotalPages != 3 || result.TotalJobs != 47 {
		t.Errorf("page = %d/%d, total %d; want 2/3, 47", result.Page, result.TotalPages, result.TotalJobs)
	}
	if len(result.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1 (duplicates removed)", len(result.Jobs))
	}

	job := result.Jobs[0]
	want := Job{
		ID:          slug,
		Source:      Source,
		Title:       "Principal Golang Engineer in HCM/Hanoi",
		URL:         "https://itviec.com/viec-lam-it/" + slug,
		Company:     "MONEY FORWARD VIETNAM CO.,LTD",
		CompanyURL:  "https://itviec.com/nha-tuyen-dung/money-forward",
		CompanyLogo: "https://itviec.com/logo.png",
		Salary:      "",
		Location:    "Hà Nội",
		WorkType:    "Linh hoạt",
		Expertise:   "Backend Developer",
		Tags:        []string{"Golang", "AI"},
		Highlights:  []string{"Hybrid working; Flexible time; Macbook Pro", "L&D programs; 17 days off"},
		PostedAt:    "Đăng 6 ngày trước",
		ScrapedAt:   job.ScrapedAt,
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job mismatch\n got: %+v\nwant: %+v", job, want)
	}
}

func TestParseSearchPageWithoutPagination(t *testing.T) {
	result, err := ParseSearchPage(strings.NewReader(samplePage(2, 0, 0, "a-1", "b-2")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if len(result.Jobs) != 2 || result.Page != 1 || result.TotalPages != 1 {
		t.Errorf("got %d jobs, page %d/%d; want 2 jobs, page 1/1", len(result.Jobs), result.Page, result.TotalPages)
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "http://localhost:1234/")
	if got, want := c.SearchURL("Frontend Developer", 1), "http://localhost:1234/it-jobs/frontend-developer/ha-noi?locale=vi"; got != want {
		t.Errorf("SearchURL page 1 = %q, want %q", got, want)
	}
	if got, want := c.SearchURL("golang", 3), "http://localhost:1234/it-jobs/golang/ha-noi?locale=vi&page=3"; got != want {
		t.Errorf("SearchURL page 3 = %q, want %q", got, want)
	}
}

func TestFetchAllJobs(t *testing.T) {
	pages := map[string]string{
		"":  samplePage(3, 1, 2, "a-1", "b-2"),
		"2": samplePage(3, 2, 2, "b-2", "c-3"),
	}
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/it-jobs/golang/ha-noi" || r.URL.Query().Get("locale") != "vi" {
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
	if !reflect.DeepEqual(ids, []string{"a-1", "b-2", "c-3"}) {
		t.Errorf("ids = %v, want [a-1 b-2 c-3]", ids)
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

	if _, err := newTestClient(srv).FetchJobs(context.Background(), "golang", 1); err == nil {
		t.Fatal("expected error on 400")
	}
}
