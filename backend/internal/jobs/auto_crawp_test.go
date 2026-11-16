package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"notification-job-mb/internal/services"
)

var errFakeBlocked = errors.New("fake: blocked")

// fakeSite serves pages[page-1] (IDs) for every keyword and records which
// pages and details were requested.
func fakeSite(pages [][]string, totalPages int) (*site[string, string], *[]int, *[]string) {
	var searched []int
	var detailed []string
	s := &site[string, string]{
		name: "fake",
		search: func(_ context.Context, _ string, page int) ([]string, int, error) {
			searched = append(searched, page)
			if page > len(pages) {
				return pages[len(pages)-1], totalPages, nil // sites repeat the last page
			}
			return pages[page-1], totalPages, nil
		},
		jobID: func(id string) string { return id },
		details: func(_ context.Context, ids []string) ([]string, error) {
			detailed = append(detailed, ids...)
			return ids, nil
		},
		convert: func(id string) services.AnalyzedJob { return services.AnalyzedJob{ID: id, Title: "t"} },
		blocked: errFakeBlocked,
	}
	return s, &searched, &detailed
}

func knownSet(ids ...string) knownFunc {
	return func(_ context.Context, in []string) (map[string]struct{}, error) {
		found := map[string]struct{}{}
		for _, id := range in {
			for _, k := range ids {
				if id == k {
					found[id] = struct{}{}
				}
			}
		}
		return found, nil
	}
}

func TestSiteCrawl(t *testing.T) {
	tests := []struct {
		name         string
		pages        [][]string
		totalPages   int
		known        []string
		wantSearched int
		wantDetailed []string
	}{
		{
			name:         "stops at max pages",
			pages:        [][]string{{"1"}, {"2"}, {"3"}, {"4"}, {"5"}, {"6"}},
			totalPages:   10,
			wantSearched: 5,
			wantDetailed: []string{"1", "2", "3", "4", "5"},
		},
		{
			name:         "stops at empty page",
			pages:        [][]string{{"1", "2"}, {}},
			totalPages:   10,
			wantSearched: 2,
			wantDetailed: []string{"1", "2"},
		},
		{
			name:         "stops at repeated page",
			pages:        [][]string{{"1"}, {"2"}},
			totalPages:   10,
			wantSearched: 3,
			wantDetailed: []string{"1", "2"},
		},
		{
			name:         "skips known jobs but keeps paging",
			pages:        [][]string{{"1", "2"}, {"3", "4"}, {"5"}},
			totalPages:   3,
			known:        []string{"fake:1", "fake:2", "fake:4"},
			wantSearched: 3,
			wantDetailed: []string{"3", "5"},
		},
		{
			name:         "nothing new fetches no details",
			pages:        [][]string{{"1"}},
			totalPages:   1,
			known:        []string{"fake:1"},
			wantSearched: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, searched, detailed := fakeSite(tt.pages, tt.totalPages)
			out, err := s.crawl(context.Background(), "go", 5, map[string]struct{}{}, knownSet(tt.known...))
			if err != nil {
				t.Fatal(err)
			}
			if len(*searched) != tt.wantSearched {
				t.Errorf("searched pages %v, want %d", *searched, tt.wantSearched)
			}
			if len(*detailed) != len(tt.wantDetailed) {
				t.Fatalf("detailed %v, want %v", *detailed, tt.wantDetailed)
			}
			for i, id := range tt.wantDetailed {
				if (*detailed)[i] != id || out[i].ID != id || out[i].Source != "fake" {
					t.Errorf("job %d = %+v, want id %s", i, out[i], id)
				}
			}
		})
	}
}

func TestSiteCrawlSharesSeenAcrossKeywords(t *testing.T) {
	s, searched, detailed := fakeSite([][]string{{"1"}, {"2"}}, 2)
	seen := map[string]struct{}{}
	if _, err := s.crawl(context.Background(), "go", 5, seen, knownSet()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.crawl(context.Background(), "golang", 5, seen, knownSet()); err != nil {
		t.Fatal(err)
	}
	if len(*detailed) != 2 {
		t.Errorf("detailed %v, want each job once", *detailed)
	}
	if len(*searched) != 4 {
		t.Errorf("searched %v, want both pages for each keyword", *searched)
	}
}

func TestSiteCrawlBlockedSkipsDetails(t *testing.T) {
	s, _, detailed := fakeSite([][]string{{"1"}}, 3)
	search := s.search
	s.search = func(ctx context.Context, kw string, page int) ([]string, int, error) {
		if page == 2 {
			return nil, 0, errFakeBlocked
		}
		return search(ctx, kw, page)
	}
	_, err := s.crawl(context.Background(), "go", 5, map[string]struct{}{}, knownSet())
	if !s.isBlocked(err) {
		t.Fatalf("err = %v, want blocked", err)
	}
	if len(*detailed) != 0 {
		t.Errorf("detailed %v after block, want none", *detailed)
	}
}

func TestNextRun(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	tests := []struct{ now, want time.Time }{
		{time.Date(2026, 10, 23, 1, 30, 0, 0, loc), time.Date(2026, 10, 23, 3, 0, 0, 0, loc)},
		{time.Date(2026, 10, 23, 3, 0, 0, 0, loc), time.Date(2026, 10, 23, 6, 0, 0, 0, loc)},
		{time.Date(2026, 10, 23, 22, 15, 0, 0, loc), time.Date(2026, 10, 24, 0, 0, 0, 0, loc)},
	}
	for _, tt := range tests {
		if got := nextRun(tt.now, 3*time.Hour); !got.Equal(tt.want) {
			t.Errorf("nextRun(%s) = %s, want %s", tt.now, got, tt.want)
		}
	}
}

func TestPendingIDs(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAutoCrawl(nil, AutoCrawlConfig{DataDir: dir, Sources: []string{"topcv"}})
	if err != nil {
		t.Fatal(err)
	}
	file := services.JobFile{Source: "topcv", Jobs: []services.AnalyzedJob{{ID: "1"}, {ID: "2", Source: "ITviec"}}}
	if err := writeJSONFile(filepath.Join(dir, rawDir, "topcv_x.json"), file); err != nil {
		t.Fatal(err)
	}
	got := a.pendingIDs()
	for _, id := range []string{"topcv:1", "itviec:2"} {
		if _, ok := got[id]; !ok {
			t.Errorf("pending %v, missing %s", got, id)
		}
	}
}

// LinkedIn is off unless ConfigureSites enables it (these tests never do, so no
// request can leave for a site that the ISP blocks).
func TestLinkedInDisabledByDefault(t *testing.T) {
	_, err := NewAutoCrawl(nil, AutoCrawlConfig{DataDir: t.TempDir(), Sources: []string{"LinkedIn"}})
	if err == nil || !strings.Contains(err.Error(), "LINKEDIN_ENABLED") {
		t.Fatalf("err = %v, want a hint to set LINKEDIN_ENABLED", err)
	}
	for _, s := range defaultSites() {
		if s.sourceName() == "linkedin" {
			t.Fatal("linkedin must not be crawled when disabled")
		}
	}
	if err := ConfigureSites(SiteOptions{LinkedIn: true}); err == nil {
		t.Error("ConfigureSites after the clients exist must fail instead of being ignored")
	}
}

func TestConfigureSitesChecksWireGuardConfig(t *testing.T) {
	err := ConfigureSites(SiteOptions{LinkedIn: true, LinkedInWireGuardConfig: t.TempDir() + "/missing.conf"})
	if err == nil {
		t.Fatal("want an error for a missing WireGuard config")
	}
}
