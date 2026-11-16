package indeed

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

// newTestClient talks to srv without any politeness delays.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.Client(), srv.URL)
	c.SetRequestDelay(0, 0)
	c.retryBase = 0
	c.batchPause = 0
	return c
}

// testdata/search.html is https://vn.indeed.com/jobs?q=kế+toán&l=Hà+Nội&sort=date reduced to
// the two script variables the adapter reads, with each job card and the side pane trimmed to
// the fields it uses. testdata/login-wall.html is what ?start=10 shows to anonymous visitors.
func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSearchPage(t *testing.T) {
	r, err := ParseSearchPage(strings.NewReader(readFile(t, "search.html")))
	if err != nil {
		t.Fatalf("ParseSearchPage: %v", err)
	}
	if r.Page != 1 || r.TotalPages != 5 || r.TotalJobs != 7480 || len(r.Jobs) != 15 {
		t.Fatalf("page %d/%d, total %d, %d jobs; want 1/5, 7480, 15", r.Page, r.TotalPages, r.TotalJobs, len(r.Jobs))
	}

	j := r.Jobs[1]
	posted := time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC)
	checks := []struct{ name, got, want string }{
		{"id", j.ID, "067c1c2257e2a1ad"},
		{"source", j.Source, Source},
		{"title", j.Title, "Creative Manager (Art & Visual Direction)"},
		{"url", j.URL, "https://vn.indeed.com/viewjob?jk=067c1c2257e2a1ad"},
		{"company", j.Company, "Công Ty TNHH Sun Rock"},
		{"salary", j.Salary, "20.000.000 đ - 25.000.000 đ / năm"},
		{"location", j.Location, "Hà Nội"},
		{"work_type", j.WorkType, "Toàn thời gian"},
		{"updated_at", j.UpdatedAt, "Vừa đăng"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if j.PostedAt == nil || !j.PostedAt.Equal(posted) {
		t.Errorf("posted_at = %v, want %v", j.PostedAt, posted)
	}
	if !strings.HasPrefix(j.Snippet, "- Phối hợp với Brand/Marketing") {
		t.Errorf("snippet = %q", j.Snippet)
	}
	if r.Jobs[0].Salary != "" {
		t.Errorf("job without salary got %q", r.Jobs[0].Salary)
	}
}

func TestParseSearchPageWithoutJobCards(t *testing.T) {
	if _, err := ParseSearchPage(strings.NewReader(readFile(t, "login-wall.html"))); err == nil {
		t.Fatal("expected error for a page without job cards")
	}
}

func TestSearchURL(t *testing.T) {
	c := NewClient(nil, "")
	if got, want := c.SearchURL("Lập trình viên Golang", 1), "https://vn.indeed.com/jobs?q=L%E1%BA%ADp+tr%C3%ACnh+vi%C3%AAn+Golang&l=H%C3%A0+N%E1%BB%99i"; got != want {
		t.Errorf("page 1 = %q, want %q", got, want)
	}
	if got, want := c.SearchURL("golang", 3), "https://vn.indeed.com/jobs?q=golang&l=H%C3%A0+N%E1%BB%99i&start=20"; got != want {
		t.Errorf("page 3 = %q, want %q", got, want)
	}
}

// TestFetchAllJobsStopsAtLoginWall: page 2 shows the login wall, which ends the walk quietly.
func TestFetchAllJobsStopsAtLoginWall(t *testing.T) {
	search, wall := readFile(t, "search.html"), readFile(t, "login-wall.html")
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("start"))
		if r.URL.Query().Get("q") != "kế toán" || r.URL.Query().Get("l") != "Hà Nội" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		if r.URL.Query().Has("start") {
			w.Write([]byte(wall))
			return
		}
		w.Write([]byte(search))
	}))
	defer srv.Close()

	jobs, err := newTestClient(srv).FetchAllJobs(context.Background(), "kế toán", 0)
	if err != nil {
		t.Fatalf("FetchAllJobs: %v", err)
	}
	if len(jobs) != 15 || strings.Join(starts, ",") != ",10" {
		t.Errorf("got %d jobs from starts %q; want 15 from \",10\"", len(jobs), starts)
	}
}

func TestFetchJobsLoginWallOnFirstPage(t *testing.T) {
	wall := readFile(t, "login-wall.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(wall))
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).FetchAllJobs(context.Background(), "golang", 1); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v, want ErrLoginRequired", err)
	}
}

func TestFetchJobsBadStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusForbidden, ErrBlocked},    // Cloudflare "Additional Verification Required"
		{http.StatusUnauthorized, ErrBlocked}, // Indeed "Authenticating..." bot detection
		{http.StatusNotFound, ErrNotFound},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := newTestClient(srv).FetchJobs(context.Background(), "golang", 1)
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.code, err, tc.want)
		}
	}
}
