package onetwothreejob

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// Source is the job-site name, the prefix of jobs.unique_identifier ("123job:5683942").
const Source = "123job"

// jobsPerPage is how many job cards a 123job search page holds.
const jobsPerPage = 30

// Job is a single job card from a 123job search result page.
//
// The site is styled with utility classes only (Tailwind), so the parser hangs on what
// has meaning: the "save" button's data-job-id, the /viec-lam/ link, rel="next".
type Job struct {
	ID          string    `json:"id"` // numeric job ID, e.g. "5683942"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"` // e.g. https://123job.vn/viec-lam/backend-developer-4qQ4onVorX
	Company     string    `json:"company"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`   // e.g. "10 - 15 triệu"; "" when hidden ("__")
	Location    string    `json:"location"` // e.g. "Tây Hồ, Hà Nội"
	PostedAgo   string    `json:"posted_ago,omitempty"`
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

var (
	totalRe  = regexp.MustCompile(`^([\d.,]+)\s+việc làm$`)
	spacesRe = regexp.MustCompile(`\s+`)
)

// ParseSearchPage parses the HTML of a 123job search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	// Each card holds a "save" button with the job ID; "similar jobs" blocks elsewhere use
	// another action (toggleSaveJob), so they are not picked up.
	doc.Find("button[data-vue-action='toggleFav'][data-job-id]").Each(func(_ int, btn *goquery.Selection) {
		id := strings.TrimSpace(btn.AttrOr("data-job-id", ""))
		card := btn.Parent()
		link := card.Find("a[href*='/viec-lam/']").First()
		jobURL := absURL(link.AttrOr("href", ""))
		if id == "" || jobURL == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		job := Job{
			ID:        id,
			Source:    Source,
			Title:     htmltext.Clean(link.Text()),
			URL:       jobURL,
			Company:   htmltext.Clean(card.Find("p").First().Text()),
			ScrapedAt: now,
		}
		if logo := card.Find("img").First().AttrOr("src", ""); logo != "" && !strings.Contains(logo, "no_company") {
			job.CompanyLogo = absURL(logo)
		}
		// Meta row: <span><span class="…orange…">salary</span></span> · <span>location</span>
		salary := card.Find("span.text-orange-500").First()
		job.Salary = salaryText(salary.Text())
		salary.Parent().Parent().ChildrenFiltered("span").Each(func(i int, s *goquery.Selection) {
			text := htmltext.Clean(s.Text())
			if i == 0 || text == "·" || text == "" {
				return
			}
			job.Location = text
		})
		card.Find("span").EachWithBreak(func(_ int, s *goquery.Selection) bool {
			if text := htmltext.Clean(s.Text()); strings.HasPrefix(text, "Đăng:") {
				job.PostedAgo = strings.TrimSpace(strings.TrimPrefix(text, "Đăng:"))
				return false
			}
			return true
		})
		result.Jobs = append(result.Jobs, job)
	})

	// "1121 việc làm" above the list.
	doc.Find("span").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if m := totalRe.FindStringSubmatch(htmltext.Clean(s.Text())); m != nil {
			result.TotalJobs, _ = strconv.Atoi(strings.NewReplacer(".", "", ",", "").Replace(m[1]))
			return false
		}
		return true
	})
	result.hasNext = doc.Find("a[rel='next']").Length() > 0
	return result, nil
}

// salaryText cleans a displayed salary; "__" and "Đang cập nhật" mean not given.
func salaryText(s string) string {
	s = spacesRe.ReplaceAllString(htmltext.Clean(s), " ")
	switch strings.ToLower(s) {
	case "", "__", "đang cập nhật":
		return ""
	}
	return s
}
