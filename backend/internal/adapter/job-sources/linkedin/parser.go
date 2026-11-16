package linkedin

import (
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "linkedin"

// pageSize is how many job cards one guest search request returns.
const pageSize = 10

// Job is a single job card from a LinkedIn search result page.
type Job struct {
	ID          string    `json:"id"` // numeric job ID, e.g. "4486584510"
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	URL         string    `json:"url"` // https://www.linkedin.com/jobs/view/<id>
	Company     string    `json:"company"`
	CompanyURL  string    `json:"company_url,omitempty"`
	CompanyLogo string    `json:"company_logo,omitempty"`
	Salary      string    `json:"salary,omitempty"` // rarely shown in Vietnam
	Location    string    `json:"location"`         // e.g. "Hanoi, Hanoi, Vietnam"
	Benefits    []string  `json:"benefits,omitempty"`
	PostedAt    string    `json:"posted_at,omitempty"`  // yyyy-mm-dd
	PostedAgo   string    `json:"posted_ago,omitempty"` // e.g. "1 week ago"
	ScrapedAt   time.Time `json:"scraped_at"`
}

// SearchResult is one parsed search page. LinkedIn does not tell signed-out clients how
// many results there are; FetchJobs fills TotalPages from whether the page was full.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	Jobs       []Job  `json:"jobs"`

	cards int // job cards on the page, duplicates and cards without an ID included
}

var (
	jobURNRe    = regexp.MustCompile(`urn:li:jobPosting:(\d+)`)
	jobPathIDRe = regexp.MustCompile(`/jobs/view/(?:[^/?#]*-)?(\d+)`)
)

// ParseSearchPage parses the HTML fragment returned by URL_FIND_JOB: a list of
// <li> job cards, or nothing past the last result.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &SearchResult{Jobs: []Job{}}
	seen := make(map[string]struct{})

	// The card is a <div> with a full-size link inside, or (some layouts) the <a> itself.
	doc.Find(".base-search-card, .job-search-card, [data-entity-urn^='urn:li:jobPosting:']").Each(func(_ int, s *goquery.Selection) {
		if s.ParentsFiltered(".base-search-card, .job-search-card").Length() > 0 {
			return // nested match inside a card already handled
		}
		result.cards++

		link := s.Find("a.base-card__full-link").First()
		href := link.AttrOr("href", "")
		if href == "" && goquery.NodeName(s) == "a" {
			href = s.AttrOr("href", "")
		}
		id := jobID(s.AttrOr("data-entity-urn", ""), href)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}

		title := htmltext.Clean(s.Find(".base-search-card__title").First().Text())
		if title == "" {
			title = htmltext.Clean(link.Find(".sr-only").First().Text())
		}
		company := s.Find(".base-search-card__subtitle").First()
		companyLink := company.Find("a").First()
		logo := s.Find(".search-entity-media img, img.artdeco-entity-image").First()
		listDate := s.Find("time").First()

		var benefits []string
		s.Find(".job-posting-benefits__text").Each(func(_ int, b *goquery.Selection) {
			benefits = appendUnique(benefits, htmltext.Clean(b.Text()))
		})

		result.Jobs = append(result.Jobs, Job{
			ID:          id,
			Source:      Source,
			Title:       title,
			URL:         JobURL(id),
			Company:     htmltext.Clean(company.Text()),
			CompanyURL:  cleanURL(companyLink.AttrOr("href", "")),
			CompanyLogo: logo.AttrOr("data-delayed-url", logo.AttrOr("src", "")),
			Salary:      htmltext.Clean(s.Find(".job-search-card__salary-info").First().Text()),
			Location:    htmltext.Clean(s.Find(".job-search-card__location").First().Text()),
			Benefits:    benefits,
			PostedAt:    strings.TrimSpace(listDate.AttrOr("datetime", "")),
			PostedAgo:   htmltext.Clean(listDate.Text()),
			ScrapedAt:   now,
		})
	})
	return result, nil
}

// JobURL is the canonical public page of a job.
func JobURL(id string) string {
	return defaultBaseURL + "/jobs/view/" + id
}

// jobID reads the numeric job ID from a card's data-entity-urn, or else from its link
// ("https://vn.linkedin.com/jobs/view/golang-developer-at-abc-4486584510?position=1...").
func jobID(urn, href string) string {
	if m := jobURNRe.FindStringSubmatch(urn); m != nil {
		return m[1]
	}
	if m := jobPathIDRe.FindStringSubmatch(href); m != nil {
		return m[1]
	}
	return ""
}

// cleanURL drops the tracking query (?trk=...) and fragment of a LinkedIn link.
func cleanURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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
