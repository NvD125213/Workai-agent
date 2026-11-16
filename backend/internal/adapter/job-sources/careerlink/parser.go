package careerlink

import (
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// Source is the job-site name, the prefix of jobs.unique_identifier ("careerlink:3676421").
const Source = "careerlink"

// jobsPerPage is how many job cards a CareerLink search page holds.
const jobsPerPage = 50

// Job is a single job card from a CareerLink search result page.
//
// The site is a Next.js app with CSS-module classes ("JobIndex-module__HZLWJW__salary");
// the hash part changes with each build, so the parser matches data-testid attributes and
// the stable end of the class names ([class*='__salary']).
type Job struct {
	ID          string    `json:"id"` // numeric job ID, e.g. "3676421"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"` // e.g. https://www.careerlink.vn/tim-viec-lam/chuyen-vien-…/3676421
	Company     string    `json:"company"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`   // e.g. "12 triệu - 15 triệu", "Thương lượng"
	Location    string    `json:"location"` // e.g. "Hà Nội, Hồ Chí Minh"
	CareerLevel string    `json:"career_level,omitempty"`
	UpdatedAt   string    `json:"updated_at,omitempty"` // RFC 3339, the card's "Cập nhật" time
	ScrapedAt   time.Time `json:"scraped_at"`
}

// SearchResult is one parsed search page.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	TotalJobs  int    `json:"total_jobs"`
	Jobs       []Job  `json:"jobs"`

	hasNext bool // the page links to a next one (rel="next")
}

// ParseSearchPage parses the HTML of a CareerLink search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find("[data-testid='job-results'] [data-testid='job-result-row'][data-job-id]").Each(func(_ int, card *goquery.Selection) {
		id := strings.TrimSpace(card.AttrOr("data-job-id", ""))
		link := card.Find("a[href*='/tim-viec-lam/']").First()
		jobURL := absURL(link.AttrOr("href", ""))
		if id == "" || jobURL == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		job := Job{
			ID:          id,
			Source:      Source,
			Title:       htmltext.Clean(firstNonEmpty(link.Find("h2").Text(), link.AttrOr("title", ""))),
			URL:         jobURL,
			Company:     htmltext.Clean(card.Find("a[href*='/viec-lam-cua/']").First().Text()),
			Salary:      htmltext.Clean(card.Find("[class*='__salary']").Not("[class*='__salaryRow']").First().Text()),
			CareerLevel: htmltext.Clean(card.Find("[class*='__careerLevel']").First().Text()),
			UpdatedAt:   card.Find("time[datetime]").First().AttrOr("datetime", ""),
			ScrapedAt:   now,
		}
		var places []string
		card.Find("[class*='__location'] a").Each(func(_ int, a *goquery.Selection) {
			if p := htmltext.Clean(a.Text()); p != "" {
				places = append(places, p)
			}
		})
		job.Location = strings.Join(places, ", ")
		if logo := card.Find("img").First().AttrOr("src", ""); logo != "" && !strings.Contains(logo, "/frontend-assets/") {
			job.CompanyLogo = absURL(logo)
		}
		result.Jobs = append(result.Jobs, job)
	})

	// <p class="…__count"><strong>222</strong> việc làm</p>
	count := strings.NewReplacer(".", "", ",", "").Replace(htmltext.Clean(doc.Find("[class*='__count'] strong").First().Text()))
	result.TotalJobs, _ = strconv.Atoi(count)
	result.hasNext = doc.Find("nav[aria-label='pagination'] a[rel='next']").Length() > 0
	return result, nil
}
