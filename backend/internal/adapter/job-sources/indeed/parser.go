package indeed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "indeed"

// Indeed renders its pages from JSON assigned in inline scripts; these are the assignments read.
const (
	jobCardsVar    = `window.mosaic.providerData["mosaic-provider-jobcards"]=`
	initialDataVar = `window._initialData=`
)

// Job is a single job posting scraped from an Indeed search result page.
type Job struct {
	ID         string     `json:"id"` // Indeed's job key (jk)
	Source     string     `json:"source"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	Company    string     `json:"company"`
	CompanyURL string     `json:"company_url,omitempty"`
	Salary     string     `json:"salary"`
	Location   string     `json:"location"`
	WorkType   string     `json:"work_type,omitempty"` // e.g. "Toàn thời gian"
	Snippet    string     `json:"snippet,omitempty"`   // short excerpt of the description
	Sponsored  bool       `json:"sponsored,omitempty"`
	PostedAt   *time.Time `json:"posted_at,omitempty"`
	UpdatedAt  string     `json:"updated_at,omitempty"` // relative text, e.g. "Vừa đăng", "30+ ngày trước"
	ScrapedAt  time.Time  `json:"scraped_at"`
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

// ViewJobURL is the public link of a job, as opened from a browser.
func ViewJobURL(jobKey string) string {
	return defaultBaseURL + "/viewjob?jk=" + url.QueryEscape(jobKey)
}

type jobCardsData struct {
	MetaData struct {
		Model struct {
			PageNumber int       `json:"pageNumber"`
			Results    []jobCard `json:"results"`
		} `json:"mosaicProviderJobCardsModel"`
	} `json:"metaData"`
}

type jobCard struct {
	JobKey              string `json:"jobkey"`
	DisplayTitle        string `json:"displayTitle"`
	Title               string `json:"title"`
	Company             string `json:"company"`
	CompanyOverviewLink string `json:"companyOverviewLink"`
	FormattedLocation   string `json:"formattedLocation"`
	SalarySnippet       struct {
		Text string `json:"text"`
	} `json:"salarySnippet"`
	Snippet            string `json:"snippet"`
	TaxonomyAttributes []struct {
		Label      string `json:"label"`
		Attributes []struct {
			Label string `json:"label"`
		} `json:"attributes"`
	} `json:"taxonomyAttributes"`
	FormattedRelativeTime string `json:"formattedRelativeTime"`
	PubDate               int64  `json:"pubDate"` // Unix milliseconds
	Sponsored             bool   `json:"sponsored"`
}

// initialData is the part of window._initialData the adapter uses.
type initialData struct {
	TotalJobCount int `json:"totalJobCount"`
	PageNum       int `json:"pageNum"`
	PageLinks     []struct {
		Label any `json:"label"` // page number, or text for prev/next links
	} `json:"pageLinks"`
	AutoOpenTwoPaneViewjobResponse *struct {
		Body *viewJobBody `json:"body"`
	} `json:"autoOpenTwoPaneViewjobResponse"`
}

// ParseSearchPage parses the HTML of an Indeed search result page.
func ParseSearchPage(r io.Reader) (*SearchResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	page := string(b)

	var cards jobCardsData
	if err := decodeScriptVar(page, jobCardsVar, &cards); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	model := cards.MetaData.Model
	result := &SearchResult{Page: model.PageNumber, Jobs: []Job{}}
	seen := make(map[string]struct{})
	for _, c := range model.Results {
		if c.JobKey == "" {
			continue
		}
		if _, ok := seen[c.JobKey]; ok {
			continue
		}
		seen[c.JobKey] = struct{}{}
		result.Jobs = append(result.Jobs, c.toJob(now))
	}

	// The total and the pager live in window._initialData; a page without them is still usable.
	var init initialData
	if decodeScriptVar(page, initialDataVar, &init) == nil {
		result.TotalJobs = init.TotalJobCount
		if result.Page == 0 {
			result.Page = init.PageNum
		}
		for _, l := range init.PageLinks {
			if n, ok := l.Label.(float64); ok {
				result.TotalPages = max(result.TotalPages, int(n))
			}
		}
	}
	if result.TotalPages == 0 && len(result.Jobs) > 0 {
		result.TotalPages = 1
	}
	return result, nil
}

func (c jobCard) toJob(now time.Time) Job {
	title := htmltext.Clean(c.DisplayTitle)
	if title == "" {
		title = htmltext.Clean(c.Title)
	}
	job := Job{
		ID:         c.JobKey,
		Source:     Source,
		Title:      title,
		URL:        ViewJobURL(c.JobKey),
		Company:    htmltext.Clean(c.Company),
		CompanyURL: absURL(c.CompanyOverviewLink),
		Salary:     htmltext.Clean(c.SalarySnippet.Text),
		Location:   htmltext.Clean(c.FormattedLocation),
		Snippet:    htmltext.FromHTML(c.Snippet),
		Sponsored:  c.Sponsored,
		UpdatedAt:  htmltext.Clean(c.FormattedRelativeTime),
		ScrapedAt:  now,
	}
	if c.PubDate > 0 {
		t := time.UnixMilli(c.PubDate).UTC()
		job.PostedAt = &t
	}
	for _, a := range c.TaxonomyAttributes {
		if a.Label != "job-types" {
			continue
		}
		var types []string
		for _, v := range a.Attributes {
			types = appendUnique(types, htmltext.Clean(v.Label))
		}
		job.WorkType = strings.Join(types, ", ")
	}
	return job
}

// decodeScriptVar decodes the JSON value assigned right after prefix (e.g. `window._initialData=`).
// The decoder stops at the end of the value, so the rest of the script is ignored.
func decodeScriptVar(page, prefix string, v any) error {
	i := strings.Index(page, prefix)
	if i < 0 {
		return fmt.Errorf("%s not found on page", strings.TrimSuffix(prefix, "="))
	}
	if err := json.NewDecoder(strings.NewReader(page[i+len(prefix):])).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", strings.TrimSuffix(prefix, "="), err)
	}
	return nil
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
