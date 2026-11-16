package careerviet

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "careerviet"

// jobsPerPage is how many job cards a CareerViet search page holds.
const jobsPerPage = 50

// Job is a single job card from a CareerViet search result page.
type Job struct {
	ID          string    `json:"id"` // hex job ID, e.g. "35C8B46A"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"` // the title link, e.g. https://careerviet.vn/vi/tim-viec-lam/video-editor.35C8B46A.html
	Company     string    `json:"company"`
	CompanyURL  string    `json:"company_url,omitempty"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`                // e.g. "12 - 15 Triệu VND", "Cạnh tranh"
	Location    string    `json:"location"`              // cities, comma separated
	Deadline    string    `json:"deadline,omitempty"`    // yyyy-mm-dd ("Hạn nộp")
	DeadlineIn  string    `json:"deadline_in,omitempty"` // relative, e.g. "3 ngày" ("Cơ hội ứng tuyển chỉ còn")
	UpdatedAt   string    `json:"updated_at,omitempty"`  // yyyy-mm-dd ("Cập nhật")
	Industries  []string  `json:"industries,omitempty"`  // e.g. "CNTT - Phần mềm"
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

var (
	jobIDRe      = regexp.MustCompile(`\.([0-9A-Fa-f]{6,})\.html`)
	leadingIntRe = regexp.MustCompile(`^\d[\d.,]*`)
	dmyRe        = regexp.MustCompile(`^(\d{1,2})[-/.](\d{1,2})[-/.](\d{4})$`)
)

// ParseSearchPage parses the HTML of a CareerViet search result page. A page past the
// last one has the same header but no job cards.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find("div.job-item[id^='job-item-']").Each(func(_ int, s *goquery.Selection) {
		titleLink := s.Find(".title a.job_link").First()
		jobURL := absURL(titleLink.AttrOr("href", ""))
		id := strings.TrimSpace(titleLink.AttrOr("data-id", ""))
		if id == "" {
			id = strings.TrimPrefix(s.AttrOr("id", ""), "job-item-")
		}
		if id == "" {
			id = jobID(jobURL)
		}
		if id == "" || jobURL == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		company := s.Find("a.company-name").First()
		logo := s.Find("img.image-logo-emp").First()

		var cities []string
		s.Find(".location li").Each(func(_ int, li *goquery.Selection) {
			cities = appendUnique(cities, htmltext.Clean(li.Text()))
		})
		location := strings.Join(cities, ", ")
		if location == "" {
			location = htmltext.Clean(s.Find(".location").First().AttrOr("title", ""))
		}

		job := Job{
			ID:          id,
			Source:      Source,
			Title:       htmltext.Clean(titleLink.Text()),
			URL:         jobURL,
			Company:     htmltext.Clean(company.Text()),
			CompanyURL:  absURL(company.AttrOr("href", "")),
			CompanyLogo: imageURL(logo.AttrOr("src", "")),
			Salary:      salaryText(s.Find(".salary").First().Text()),
			Location:    location,
			ScrapedAt:   now,
		}
		if job.Title == "" {
			job.Title = htmltext.Clean(titleLink.AttrOr("title", ""))
		}
		// "Hạn nộp: 07-12-2026", "Cơ hội ứng tuyển chỉ còn: 3 ngày", "Cập nhật: 12-11-2026"
		s.Find(".time li").Each(func(_ int, li *goquery.Selection) {
			label := strings.ToLower(htmltext.Clean(li.Find("span").First().Text()))
			value := htmltext.Clean(li.Find("time").First().Text())
			switch {
			case strings.HasPrefix(label, "hạn nộp"):
				job.Deadline = isoDate(value)
			case strings.HasPrefix(label, "cơ hội ứng tuyển"):
				job.DeadlineIn = value
			case strings.HasPrefix(label, "cập nhật"):
				job.UpdatedAt = isoDate(value)
			}
		})
		s.Find("ul.welfare li").Each(func(_ int, li *goquery.Selection) {
			job.Industries = appendUnique(job.Industries, htmltext.Clean(li.Text()))
		})
		result.Jobs = append(result.Jobs, job)
	})

	// Header: "437 việc làm developer".
	header := htmltext.Clean(doc.Find("h1").First().Text())
	if m := leadingIntRe.FindString(header); m != "" {
		result.TotalJobs, _ = strconv.Atoi(strings.NewReplacer(".", "", ",", "").Replace(m))
	}

	// The pager only renders up to 5 numbers; the total from the header is more reliable.
	doc.Find(".pagination li").Each(func(_ int, li *goquery.Selection) {
		n, err := strconv.Atoi(htmltext.Clean(li.Text()))
		if err != nil {
			return
		}
		if li.HasClass("active") {
			result.Page = n
		}
		if n > result.TotalPages {
			result.TotalPages = n
		}
	})
	if result.TotalJobs > 0 {
		if n := (result.TotalJobs + jobsPerPage - 1) / jobsPerPage; n > result.TotalPages {
			result.TotalPages = n
		}
	}
	if result.TotalPages == 0 && len(result.Jobs) > 0 {
		result.Page, result.TotalPages = 1, 1
	}
	return result, nil
}

// jobID reads the hex ID at the end of a job URL ("/vi/tim-viec-lam/video-editor.35C8B46A.html").
func jobID(jobURL string) string {
	if m := jobIDRe.FindStringSubmatch(jobURL); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// salaryText drops the "Lương:" label of a salary.
func salaryText(s string) string {
	s = htmltext.Clean(s)
	if rest, ok := strings.CutPrefix(s, "Lương:"); ok {
		s = strings.TrimSpace(rest)
	}
	return s
}

// isoDate turns "07-12-2026" or "07/12/2026" into "2026-12-07"; other text is kept as is.
func isoDate(s string) string {
	s = strings.TrimSpace(s)
	m := dmyRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	d, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	t := time.Date(atoi(m[3]), time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != mo {
		return s // 31/02 and the like
	}
	return t.Format("2006-01-02")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
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
