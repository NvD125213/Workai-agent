package itviec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// URL_JOB_DETAIL is the job preview fragment the search page loads; {job_name} is the job slug.
var URL_JOB_DETAIL = `https://itviec.com/it-jobs/{job_name}/content`

// JobDetail is the content of an ITviec job, as plain text ready for AI matching.
// It follows topcv.JobDetail so all sources can be matched the same way.
type JobDetail struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	URL           string            `json:"url"`
	Title         string            `json:"title"`
	Company       string            `json:"company"`
	CompanyURL    string            `json:"company_url,omitempty"`
	Overview      Overview          `json:"overview"`
	TopReasons    []string          `json:"top_reasons,omitempty"` // "Top 3 reasons to join us"
	Description   string            `json:"description"`
	Requirements  string            `json:"requirements"`
	Benefits      string            `json:"benefits"`
	Skills        Skills            `json:"skills"`
	WorkLocations []string          `json:"work_locations,omitempty"` // full addresses
	CompanyInfo   map[string]string `json:"company_info,omitempty"`   // e.g. "Quy mô công ty": "301-500"
	OtherSections map[string]string `json:"other_sections,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary block at the top of a job.
type Overview struct {
	Salary      string   `json:"salary"` // empty when ITviec asks to sign in
	Location    string   `json:"location"`
	WorkType    string   `json:"work_type,omitempty"`
	DatePosted  string   `json:"date_posted,omitempty"` // relative text, e.g. "6 ngày trước"
	JobFunction string   `json:"job_function,omitempty"`
	Domains     []string `json:"domains,omitempty"` // "Lĩnh vực", e.g. "Banking"
}

type Skills struct {
	All []string `json:"all,omitempty"` // ITviec does not split required/preferred skills
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// DetailURL builds the job preview URL for a job slug.
func (c *Client) DetailURL(slug string) string {
	u := strings.Replace(URL_JOB_DETAIL, "{job_name}", url.PathEscape(slug), 1)
	return c.rebase(u) + "?locale=" + locale
}

// FetchJobDetail downloads and parses the preview of one job, identified by its slug.
func (c *Client) FetchJobDetail(ctx context.Context, slug string) (*JobDetail, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, errors.New("itviec: job slug is required")
	}
	detailURL := c.DetailURL(slug)
	body, err := c.get(ctx, detailURL, c.baseURL+"/it-jobs")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, slug)
		}
		return nil, fmt.Errorf("itviec: parse %s: %w", detailURL, err)
	}
	detail.ID = slug
	if detail.URL == "" {
		detail.URL = defaultBaseURL + "/it-jobs/" + slug
	}
	return detail, nil
}

// FetchJobDetails fetches the pages of jobs one by one, through the client's rate limiter,
// with a longer pause every batchEvery pages. onDetail (optional) is called after each success,
// so callers can persist progress. Jobs that are gone or fail to parse are reported as failures
// and skipped; a Cloudflare challenge or ctx cancellation stops the run and is returned as the
// error together with everything fetched so far.
func (c *Client) FetchJobDetails(ctx context.Context, jobs []Job, onDetail func(JobDetail)) ([]JobDetail, []DetailFailure, error) {
	var (
		details  []JobDetail
		failures []DetailFailure
	)
	for i, job := range jobs {
		if i > 0 && c.batchEvery > 0 && i%c.batchEvery == 0 {
			pause := c.batchPause + randDuration(c.batchPause/3)
			slog.Info("itviec: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.ID)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("itviec: skip job detail", "id", job.ID, "err", err)
			failures = append(failures, DetailFailure{ID: job.ID, URL: job.URL, Error: err.Error()})
			continue
		}
		detail.mergeJob(job)
		details = append(details, *detail)
		if onDetail != nil {
			onDetail(*detail)
		}
	}
	return details, failures, nil
}

// ParseJobDetail parses the job preview HTML returned by URL_JOB_DETAIL. ITviec answers
// unknown slugs with a full "Oops" page (status 200), which is reported as ErrNotFound.
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	header := doc.Find(".preview-job-header").First()
	if header.Length() == 0 {
		return nil, ErrNotFound
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	parseHeader(header, d)
	parseOverview(doc.Find("section.preview-job-overview").First(), d)

	doc.Find("section.reasons-join-us + ul li").Each(func(_ int, li *goquery.Selection) {
		d.TopReasons = appendUnique(d.TopReasons, htmltext.Clean(li.Text()))
	})
	parseSections(doc, d)
	parseCompanyInfo(doc.Find("section.company-infos").First(), d)

	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

func parseHeader(header *goquery.Selection, d *JobDetail) {
	d.Title = htmltext.Clean(header.Find("h2").First().Text())
	if link := header.Find("a:has(h2)").First(); link.Length() > 0 {
		d.URL = absURL(link.AttrOr("href", ""))
	}
	company := header.Find("span a").First()
	d.Company = htmltext.Clean(company.Text())
	d.CompanyURL = absURL(company.AttrOr("href", ""))
	d.Overview.Salary = salaryText(header.Find(".salary").First())
}

func parseOverview(s *goquery.Selection, d *JobDetail) {
	// Address lines come first, then the work type and the posting age.
	var headerItems []string
	s.Find(".d-inline-block").Each(func(_ int, item *goquery.Selection) {
		text := htmltext.Clean(item.Find("span").First().Text())
		if text == "" {
			return
		}
		if item.HasClass("preview-header-item") {
			headerItems = append(headerItems, text)
		} else {
			d.WorkLocations = appendUnique(d.WorkLocations, text)
		}
	})
	if len(headerItems) > 0 {
		d.Overview.WorkType = headerItems[0]
	}
	if len(headerItems) > 1 {
		d.Overview.DatePosted = headerItems[1]
	}

	// Tag rows: "Kỹ năng:" / "Skills:", "Chuyên môn:" / "Job Expertise:", "Lĩnh vực:" / "Job Domain:".
	s.Find(".fw-600.text-nowrap").Each(func(_ int, label *goquery.Selection) {
		var values []string
		label.Next().Find(".itag").Each(func(_ int, t *goquery.Selection) {
			// Domain tags carry a tooltip <div>; only the tag's own text is wanted.
			t = t.Clone()
			t.Find("div").Remove()
			values = appendUnique(values, htmltext.Clean(t.Text()))
		})
		switch name := strings.TrimSuffix(htmltext.Clean(label.Text()), ":"); name {
		case "Kỹ năng", "Skills":
			d.Skills.All = values
		case "Chuyên môn", "Job Expertise":
			d.Overview.JobFunction = strings.Join(values, ", ")
		case "Lĩnh vực", "Job Domain":
			d.Overview.Domains = values
		}
	})
}

func parseSections(doc *goquery.Document, d *JobDetail) {
	doc.Find(".preview-job-content > section").Each(func(_ int, s *goquery.Selection) {
		body := htmltext.FromSelection(s.Find(".paragraph").First())
		switch {
		case s.HasClass("job-description"):
			d.Description = body
		case s.HasClass("job-experiences"):
			d.Requirements = body
		case s.HasClass("job-why-love-working"):
			d.Benefits = body
		case s.HasClass("preview-job-overview"), s.HasClass("reasons-join-us"), s.HasClass("company-infos"):
		default:
			if heading := htmltext.Clean(s.Find("h2").First().Text()); heading != "" && body != "" {
				d.OtherSections = setTag(d.OtherSections, heading, body)
			}
		}
	})
}

// parseCompanyInfo pairs the label rows ("Quy mô công ty", ...) with the value rows below them.
func parseCompanyInfo(s *goquery.Selection, d *JobDetail) {
	var labels []string
	s.Find(".row").Each(func(_ int, row *goquery.Selection) {
		cells := row.Find("small")
		if cells.First().HasClass("text-dark-grey") {
			labels = labels[:0]
			cells.Each(func(_ int, c *goquery.Selection) {
				labels = append(labels, htmltext.Clean(c.Text()))
			})
			return
		}
		cells.Each(func(i int, c *goquery.Selection) {
			if i >= len(labels) {
				return
			}
			c = c.Clone()
			c.Find("[data-controller='tooltip']").Remove()
			d.CompanyInfo = setTag(d.CompanyInfo, labels[i], htmltext.Clean(c.Text()))
		})
		labels = labels[:0]
	})
}

// mergeJob fills in what only the search card has (city, expertise, highlights, logo).
func (d *JobDetail) mergeJob(j Job) {
	if j.URL != "" {
		d.URL = j.URL
	}
	if d.Title == "" {
		d.Title = j.Title
	}
	if d.Company == "" {
		d.Company = j.Company
	}
	if d.CompanyURL == "" {
		d.CompanyURL = j.CompanyURL
	}
	o := &d.Overview
	if o.Salary == "" {
		o.Salary = j.Salary
	}
	o.Location = j.Location
	if o.WorkType == "" {
		o.WorkType = j.WorkType
	}
	if o.JobFunction == "" {
		o.JobFunction = j.Expertise
	}
	if len(d.Skills.All) == 0 {
		d.Skills.All = j.Tags
	}
	if len(d.TopReasons) == 0 {
		d.TopReasons = j.Highlights
	}
}

func setTag(m map[string]string, k, v string) map[string]string {
	if k == "" || v == "" {
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}
