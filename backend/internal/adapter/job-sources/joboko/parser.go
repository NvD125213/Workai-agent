package joboko

import (
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// Source is the job-site name, the prefix of jobs.unique_identifier ("joboko:6834332").
const Source = "joboko"

// Job is a single job card from a Joboko search result page.
type Job struct {
	ID          string    `json:"id"` // numeric job ID, e.g. "6834332"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"` // e.g. https://vn.joboko.com/viec-lam-php-developer-xvi6834332
	Company     string    `json:"company"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary"`             // e.g. "16 - 22 triệu VND"
	Location    string    `json:"location"`           // provinces, e.g. "Hà Nội, Hồ Chí Minh"
	Deadline    string    `json:"deadline,omitempty"` // as given by the card, e.g. "2026-12-02T17:11:00"
	Highlights  []string  `json:"highlights,omitempty"`
	ScrapedAt   time.Time `json:"scraped_at"`
}

// SearchResult is one parsed search page.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	Jobs       []Job  `json:"jobs"`

	nextPage int // p= of the "Xem thêm việc làm" link, 0 when there is none
}

var jobIDRe = regexp.MustCompile(`-xvi(\d+)(?:$|[/?#])`)

// ParseSearchPage parses the HTML of a Joboko search result page. Only the result list
// is read: the page also has "similar jobs" and other blocks with the same card markup.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	doc.Find(".nw-job-list__list > div.item[data-jid]").Each(func(_ int, s *goquery.Selection) {
		titleLink := s.Find(".item-title a").First()
		jobURL := absURL(titleLink.AttrOr("href", ""))
		id := strings.TrimSpace(s.AttrOr("data-jid", ""))
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

		logo := s.Find(".item-logo img").First()
		date := s.Find(".item-date").First()
		deadline := strings.TrimSpace(date.AttrOr("data-value", ""))
		if deadline == "" {
			deadline = isoDate(htmltext.Clean(date.Text()))
		}
		var highlights []string
		s.Find(".extra-info li").Each(func(_ int, li *goquery.Selection) {
			highlights = appendUnique(highlights, htmltext.Clean(li.Text()))
		})

		result.Jobs = append(result.Jobs, Job{
			ID:          id,
			Source:      Source,
			Title:       htmltext.Clean(titleLink.Text()),
			URL:         jobURL,
			Company:     htmltext.Clean(s.Find(".item-company .text-capitalize").First().Text()),
			CompanyLogo: logo.AttrOr("data-lazy", logo.AttrOr("src", "")),
			Salary:      htmltext.Clean(s.Find(".item-rate").First().Text()),
			Location:    htmltext.Clean(s.Find(".item-address").First().Text()),
			Deadline:    deadline,
			Highlights:  highlights,
			ScrapedAt:   now,
		})
	})
	if href, ok := doc.Find(".nw-job-list__more a[href]").First().Attr("href"); ok {
		if u, err := url.Parse(href); err == nil {
			result.nextPage, _ = strconv.Atoi(u.Query().Get("p"))
		}
	}
	return result, nil
}

// jobID reads the numeric ID at the end of a job URL ("/viec-lam-php-developer-xvi6834332").
func jobID(jobURL string) string {
	if m := jobIDRe.FindStringSubmatch(jobURL); m != nil {
		return m[1]
	}
	return ""
}

var dmyRe = regexp.MustCompile(`(\d{1,2})/(\d{1,2})/(\d{4})`)

// isoDate turns the first "10/12/2026" in s into "2026-12-10"; "" when there is none.
func isoDate(s string) string {
	m := dmyRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	d, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	y, _ := strconv.Atoi(m[3])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != mo {
		return ""
	}
	return t.Format("2006-01-02")
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
