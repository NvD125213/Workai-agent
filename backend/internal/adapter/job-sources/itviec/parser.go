package itviec

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "itviec"

// jobsPerPage is how many job cards an ITviec search page holds.
const jobsPerPage = 20

// Job is a single job posting scraped from an ITviec search result page.
// ITviec hides salaries from signed-out visitors, so Salary is usually empty.
type Job struct {
	ID          string    `json:"id"` // job slug, e.g. "principal-golang-engineer-...-2356"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Company     string    `json:"company"`
	CompanyURL  string    `json:"company_url,omitempty"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`
	Location    string    `json:"location"`
	WorkType    string    `json:"work_type,omitempty"`  // "Tại văn phòng", "Linh hoạt", "Từ xa"
	Expertise   string    `json:"expertise,omitempty"`  // e.g. "Backend Developer"
	Tags        []string  `json:"tags,omitempty"`       // skills
	Highlights  []string  `json:"highlights,omitempty"` // "top 3 reasons" shown on the card
	PostedAt    string    `json:"posted_at,omitempty"`  // relative text, e.g. "Đăng 6 ngày trước"
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
}

var leadingIntRe = regexp.MustCompile(`^\d[\d.,]*`)

// ParseSearchPage parses the HTML of an ITviec search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find("div.job-card[data-search--job-selection-job-slug-value]").Each(func(_ int, s *goquery.Selection) {
		slug := strings.TrimSpace(s.AttrOr("data-search--job-selection-job-slug-value", ""))
		if slug == "" {
			return
		}
		if _, ok := seen[slug]; ok {
			return
		}
		seen[slug] = struct{}{}

		titleLink := s.Find("h3 a").First()
		jobURL := absURL(titleLink.AttrOr("href", ""))
		if jobURL == "" {
			jobURL = defaultBaseURL + "/it-jobs/" + slug
		}
		company := s.Find(".imy-3 span a").First()
		logo := s.Find(".logo-employer-card img").First()

		var tags []string
		s.Find("[data-controller='responsive-tag-list'] a.itag").Each(func(_ int, t *goquery.Selection) {
			tags = appendUnique(tags, htmltext.Clean(t.Text()))
		})
		var highlights []string
		s.Find("ul li").Each(func(_ int, li *goquery.Selection) {
			highlights = appendUnique(highlights, htmltext.Clean(li.Text()))
		})

		result.Jobs = append(result.Jobs, Job{
			ID:          slug,
			Source:      Source,
			Title:       htmltext.Clean(titleLink.Text()),
			URL:         jobURL,
			Company:     htmltext.Clean(company.Text()),
			CompanyURL:  absURL(company.AttrOr("href", "")),
			CompanyLogo: logo.AttrOr("data-src", logo.AttrOr("src", "")),
			Salary:      salaryText(s.Find(".salary").First()),
			Location:    htmltext.Clean(s.Find("div.text-rich-grey.text-truncate[title]").First().AttrOr("title", "")),
			WorkType:    htmltext.Clean(s.Find("div.text-rich-grey.flex-shrink-0").First().Text()),
			Expertise:   htmltext.Clean(s.Find(".imt-1 a[title]").First().AttrOr("title", "")),
			Tags:        tags,
			Highlights:  highlights,
			PostedAt:    htmltext.Clean(s.Find(".ipy-2 span.small-text").First().Text()),
			ScrapedAt:   now,
		})
	})

	// Header: "47 java jobs in Ha Noi" / "47 việc làm java tại Hà Nội".
	header := htmltext.Clean(doc.Find("h1.headline-total-jobs").First().Text())
	if m := leadingIntRe.FindString(header); m != "" {
		result.TotalJobs, _ = strconv.Atoi(strings.NewReplacer(".", "", ",", "").Replace(m))
	}

	pager := doc.Find("nav.ipagination")
	pager.Find(".page").Each(func(_ int, p *goquery.Selection) {
		n, err := strconv.Atoi(htmltext.Clean(p.Text()))
		if err != nil {
			return
		}
		if p.HasClass("current") {
			result.Page = n
		}
		if n > result.TotalPages {
			result.TotalPages = n
		}
	})
	if result.TotalPages == 0 && len(result.Jobs) > 0 {
		// No pagination block means everything fits on one page.
		result.Page, result.TotalPages = 1, 1
		if result.TotalJobs > jobsPerPage {
			result.TotalPages = (result.TotalJobs + jobsPerPage - 1) / jobsPerPage
		}
	}
	return result, nil
}

// salaryText returns the displayed salary, or "" when ITviec asks to sign in to see it.
func salaryText(s *goquery.Selection) string {
	if s.Find(".sign-in-view-salary").Length() > 0 {
		return ""
	}
	return htmltext.Clean(s.Text())
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
