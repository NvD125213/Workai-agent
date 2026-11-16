package jobsgo

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "jobsgo"

// jobsPerPage is how many job cards a JobsGO search page holds.
const jobsPerPage = 50

// Job is a single job posting scraped from a JobsGO search result page.
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
	WorkType    string    `json:"work_type,omitempty"`  // e.g. "Full-time"
	UpdatedAt   string    `json:"updated_at,omitempty"` // relative text, e.g. "1 giờ trước"
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

// totalJobsRe reads the header "Tuyển dụng 1046 việc làm Kế Toán tại Hà Nội".
var totalJobsRe = regexp.MustCompile(`(\d[\d.,]*)\s+việc làm`)

// ParseSearchPage parses the HTML of a JobsGO search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find(".job-card[data-id]").Each(func(_ int, s *goquery.Selection) {
		id := strings.TrimSpace(s.AttrOr("data-id", ""))
		titleLink := s.Find("h3.job-title a").First()
		if id == "" || titleLink.Length() == 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		title := htmltext.Clean(titleLink.AttrOr("title", ""))
		if title == "" {
			title = htmltext.Clean(titleLink.Text())
		}
		company := s.Find("a.company-title").First()

		// "<span>20 - 26 triệu VNĐ</span><span>|</span><span>Hà Nội, </span>"
		var meta []string
		s.Find(".text-primary.fw-semibold > span").Each(func(_ int, span *goquery.Selection) {
			if v := htmltext.Clean(span.Text()); v != "" && v != "|" {
				meta = append(meta, v)
			}
		})
		job := Job{
			ID:          id,
			Source:      Source,
			Title:       title,
			URL:         absURL(titleLink.AttrOr("href", "")),
			Company:     htmltext.Clean(company.Text()),
			CompanyURL:  absURL(company.AttrOr("href", "")),
			CompanyLogo: s.Find(".image-wrapper img").First().AttrOr("src", ""),
			ScrapedAt:   now,
		}
		if len(meta) > 0 {
			job.Salary = meta[0]
		}
		if len(meta) > 1 {
			job.Location = strings.TrimRight(meta[len(meta)-1], ", ")
		}
		s.Find("span.badge[title]").Each(func(_ int, b *goquery.Selection) {
			v := htmltext.Clean(b.Text())
			switch b.AttrOr("title", "") {
			case "Loại hình":
				job.WorkType = v
			case "Yêu cầu kinh nghiệm":
				job.Experience = v
			case "Thời gian cập nhật":
				job.UpdatedAt = v
			}
		})
		result.Jobs = append(result.Jobs, job)
	})

	if m := totalJobsRe.FindStringSubmatch(htmltext.Clean(doc.Find("h1").First().Text())); m != nil {
		result.TotalJobs, _ = strconv.Atoi(strings.NewReplacer(".", "", ",", "").Replace(m[1]))
	}

	// The pager only shows a window of pages, so the total also comes from the job count.
	doc.Find("ul.pagination li a").Each(func(_ int, a *goquery.Selection) {
		n, err := strconv.Atoi(htmltext.Clean(a.Text()))
		if err != nil {
			return
		}
		if a.Parent().HasClass("active") {
			result.Page = n
		}
		result.TotalPages = max(result.TotalPages, n)
	})
	if result.TotalJobs > 0 {
		result.TotalPages = max(result.TotalPages, (result.TotalJobs+jobsPerPage-1)/jobsPerPage)
	}
	if result.TotalPages == 0 && len(result.Jobs) > 0 {
		result.TotalPages = 1
	}
	return result, nil
}
