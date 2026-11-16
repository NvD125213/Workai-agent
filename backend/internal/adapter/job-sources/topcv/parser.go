package topcv

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "topcv"

// Job is a single job posting scraped from a TopCV search result page.
type Job struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Company     string    `json:"company"`
	CompanyURL  string    `json:"company_url,omitempty"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`
	Location    string    `json:"location"`
	Experience  string    `json:"experience,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	PostedAt    string    `json:"posted_at,omitempty"`  // relative text, e.g. "1 tuần trước"
	UpdatedAt   string    `json:"updated_at,omitempty"` // relative text, e.g. "Cập nhật 11 phút trước"
	ScrapedAt   time.Time `json:"scraped_at"`
}

// SearchResult is one parsed search page.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	Jobs       []Job  `json:"jobs"`
}

var pageTextRe = regexp.MustCompile(`(\d+)\s*/\s*(\d+)`)

// ParseSearchPage parses the HTML of a TopCV search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find("div.job-item-search-result[data-job-id]").Each(func(_ int, s *goquery.Selection) {
		id := strings.TrimSpace(s.AttrOr("data-job-id", ""))
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		titleLink := s.Find("h3.title a").First()
		title := htmltext.Clean(titleLink.Find("span").AttrOr("title", ""))
		if title == "" {
			title = htmltext.Clean(titleLink.Text())
		}

		company := s.Find("a.company").First()
		companyName := htmltext.Clean(company.Find(".company-name").AttrOr("title", ""))
		if companyName == "" {
			companyName = htmltext.Clean(company.Text())
		}

		logo := s.Find(".avatar img").First()
		logoURL := logo.AttrOr("data-src", logo.AttrOr("src", ""))

		var tags []string
		s.Find(".box-icon .tag .item-tag").Each(func(_ int, t *goquery.Selection) {
			if v := htmltext.Clean(t.Text()); v != "" {
				tags = append(tags, v)
			}
		})
		// The "+N" bubble keeps the hidden tags in its tooltip, comma separated.
		if more := s.Find(".box-icon .tag .remaining-items").AttrOr("title", ""); more != "" {
			for _, v := range strings.Split(more, ",") {
				if v = htmltext.Clean(v); v != "" {
					tags = append(tags, v)
				}
			}
		}

		tags = dedupeTags(tags)

		updated := s.Find(".box-icon label.label-update").First()
		posted := updated.Clone()
		posted.Find(".hidden-on-quick-view").Remove()

		result.Jobs = append(result.Jobs, Job{
			ID:          id,
			Source:      Source,
			Title:       title,
			URL:         cleanJobURL(titleLink.AttrOr("href", "")),
			Company:     companyName,
			CompanyURL:  company.AttrOr("href", ""),
			CompanyLogo: logoURL,
			Salary:      htmltext.Clean(s.Find(".info label.salary").Text()),
			Location:    htmltext.Clean(s.Find(".info .city-text").Text()),
			Experience:  htmltext.Clean(s.Find(".info label.exp").Text()),
			Tags:        tags,
			PostedAt:    htmltext.Clean(posted.Text()),
			UpdatedAt:   htmltext.Clean(updated.AttrOr("title", "")),
			ScrapedAt:   now,
		})
	})

	if m := pageTextRe.FindStringSubmatch(htmltext.Clean(doc.Find("#job-listing-paginate-text").Text())); m != nil {
		result.Page, _ = strconv.Atoi(m[1])
		result.TotalPages, _ = strconv.Atoi(m[2])
	} else if len(result.Jobs) > 0 {
		// No pagination block means everything fits on one page.
		result.Page, result.TotalPages = 1, 1
	}

	return result, nil
}

// dedupeTags drops exact duplicates and visible tags truncated with "..." whose
// full text is also present (TopCV repeats it in the "+N" tooltip).
func dedupeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := tags[:0]
	for _, t := range tags {
		if prefix, ok := strings.CutSuffix(t, "..."); ok {
			full := false
			for _, other := range tags {
				if other != t && strings.HasPrefix(other, prefix) {
					full = true
					break
				}
			}
			if full {
				continue
			}
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// Slugify converts a Vietnamese keyword into TopCV's URL slug,
// e.g. "Lập trình viên Golang" -> "lap-trinh-vien-golang".
func Slugify(keyword string) string {
	return htmltext.Slugify(keyword)
}
