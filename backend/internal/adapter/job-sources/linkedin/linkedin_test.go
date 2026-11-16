package linkedin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSearchPage(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(readFixture(t, "search.html")))
	if err != nil {
		t.Fatal(err)
	}
	if res.cards != 3 {
		t.Errorf("cards = %d, want 3 (duplicates count toward a full page)", res.cards)
	}
	if len(res.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2: %+v", len(res.Jobs), res.Jobs)
	}

	j := res.Jobs[0]
	want := Job{
		ID: "4486584510", Source: Source, Title: "Golang Developer",
		URL:         "https://www.linkedin.com/jobs/view/4486584510",
		Company:     "Acme Software",
		CompanyURL:  "https://vn.linkedin.com/company/acme",
		CompanyLogo: "https://media.licdn.com/dms/image/acme.png",
		Salary:      "$1,500 - $2,500", Location: "Hanoi, Hanoi, Vietnam",
		PostedAt: "2026-11-10", PostedAgo: "2 days ago",
	}
	j.ScrapedAt = time.Time{}
	if j.ID != want.ID || j.Title != want.Title || j.URL != want.URL || j.Company != want.Company ||
		j.CompanyURL != want.CompanyURL || j.CompanyLogo != want.CompanyLogo || j.Salary != want.Salary ||
		j.Location != want.Location || j.PostedAt != want.PostedAt || j.PostedAgo != want.PostedAgo {
		t.Errorf("job 0 =\n%+v\nwant\n%+v", j, want)
	}
	if len(j.Benefits) != 1 || j.Benefits[0] != "Actively Hiring" {
		t.Errorf("benefits = %v", j.Benefits)
	}

	// The whole card is the link: ID comes from the URL.
	j = res.Jobs[1]
	if j.ID != "4490000001" || j.Title != "Backend Engineer (Go/Java)" || j.Company != "Beta Corp" ||
		j.CompanyURL != "" || j.PostedAt != "2026-10-20" {
		t.Errorf("job 1 = %+v", j)
	}
}

func TestParseSearchPageEmpty(t *testing.T) {
	res, err := ParseSearchPage(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Jobs) != 0 || res.cards != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(readFixture(t, "job-posting.html")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Golang Developer" || d.Company != "Acme Software" || d.CompanyURL != "https://vn.linkedin.com/company/acme" {
		t.Errorf("top card = %q / %q / %q", d.Title, d.Company, d.CompanyURL)
	}
	o := d.Overview
	if o.Location != "Hanoi, Hanoi, Vietnam" || o.PostedAgo != "2 days ago" || o.Applicants != "Over 200 applicants" {
		t.Errorf("overview = %+v", o)
	}
	if o.Level != "Mid-Senior level" || o.EmploymentType != "Full-time" ||
		o.JobFunction != "Engineering and Information Technology" || o.Industries != "Software Development" {
		t.Errorf("criteria overview = %+v", o)
	}
	if len(d.Criteria) != 4 {
		t.Errorf("criteria = %v", d.Criteria)
	}
	for _, want := range []string{"We build payment systems in Go.", "3+ years of experience with Golang", "13th month salary"} {
		if !strings.Contains(d.Description, want) {
			t.Errorf("description misses %q:\n%s", want, d.Description)
		}
	}
	if strings.Contains(d.Description, "Show more") {
		t.Errorf("description has the button text:\n%s", d.Description)
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body></body></html>")); !errors.Is(err, ErrNotFound) {
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

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	got := c.SearchURL("golang developer", 3)
	want := "https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search?f_TPR=r2592000&geoId=105790653&keywords=golang+developer&start=20"
	if got != want {
		t.Errorf("SearchURL =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(c.SearchURL("go", 1), "start=") {
		t.Error("page 1 must not send start")
	}
}

func TestFetchJobsPaging(t *testing.T) {
	full := strings.Repeat(readFixture(t, "search.html"), 4) // 12 cards >= pageSize
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("start") {
		case "":
			w.Write([]byte(full))
		case "10":
			w.Write([]byte(readFixture(t, "search.html"))) // 3 cards: last page
		default:
			w.WriteHeader(http.StatusBadRequest) // LinkedIn past the end
		}
	}))
	ctx := context.Background()

	r1, err := c.FetchJobs(ctx, "golang", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r1.TotalPages != 2 || len(r1.Jobs) != 2 {
		t.Errorf("page 1: total %d, jobs %d", r1.TotalPages, len(r1.Jobs))
	}
	r2, err := c.FetchJobs(ctx, "golang", 2)
	if err != nil || r2.TotalPages != 2 {
		t.Errorf("page 2: %+v, %v", r2, err)
	}
	r3, err := c.FetchJobs(ctx, "golang", 3)
	if err != nil || len(r3.Jobs) != 0 {
		t.Errorf("page 3 past the end: %+v, %v", r3, err)
	}
}

func TestBlocked(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"999": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(999) },
		"authwall": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/authwall") {
				w.Write([]byte("<html>sign in</html>"))
				return
			}
			http.Redirect(w, r, "/authwall?trk=x", http.StatusFound)
		},
	} {
		var calls atomic.Int32
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			h(w, r)
		}))
		if _, err := c.FetchJobs(context.Background(), "golang", 1); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: err = %v, want ErrBlocked", name, err)
		}
		if name == "999" && calls.Load() != 1 {
			t.Errorf("%s: %d calls, a block must not be retried", name, calls.Load())
		}
	}
}

func TestRetryOn429(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(readFixture(t, "job-posting.html")))
	}))
	d, err := c.FetchJobDetail(context.Background(), "4486584510")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || d.ID != "4486584510" || d.URL != "https://www.linkedin.com/jobs/view/4486584510" {
		t.Errorf("calls %d, detail %s %s", calls.Load(), d.ID, d.URL)
	}
}

func TestFetchJobDetailsMergesCard(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(readFixture(t, "job-posting.html")))
	}))
	jobs := []Job{
		{ID: "4486584510", PostedAt: "2026-11-10", Salary: "$1,500 - $2,500"},
		{ID: "gone"},
	}
	details, failures, err := c.FetchJobDetails(context.Background(), jobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 1 || len(failures) != 1 || failures[0].ID != "gone" {
		t.Fatalf("details %d, failures %+v", len(details), failures)
	}
	if o := details[0].Overview; o.DatePosted != "2026-11-10" || o.Salary != "$1,500 - $2,500" {
		t.Errorf("merged overview = %+v", o)
	}
}
