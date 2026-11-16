package linkedin

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

// URL_JOB_DETAIL is the signed-out job posting fragment; {job_id} is the numeric job ID.
var URL_JOB_DETAIL = defaultBaseURL + "/jobs-guest/jobs/api/jobPosting/{job_id}"

// JobDetail is the content of a LinkedIn job, as plain text ready for AI matching.
// It follows topcv.JobDetail so all sources can be matched the same way. LinkedIn puts
// the whole posting in one block, so it all goes to Description; Requirements and
// Benefits stay empty, and signed-out pages show no skills.
type JobDetail struct {
	ID           string            `json:"id"`
	Source       string            `json:"source"`
	URL          string            `json:"url"`
	Title        string            `json:"title"`
	Company      string            `json:"company"`
	CompanyURL   string            `json:"company_url,omitempty"`
	Overview     Overview          `json:"overview"`
	Description  string            `json:"description"`
	Requirements string            `json:"requirements"`
	Benefits     string            `json:"benefits"`
	Skills       Skills            `json:"skills"`
	Criteria     map[string]string `json:"criteria,omitempty"` // every "job criteria" row, by label
	ScrapedAt    time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job: top card plus the "job criteria" list.
type Overview struct {
	Salary         string `json:"salary,omitempty"`
	Location       string `json:"location"`
	DatePosted     string `json:"date_posted,omitempty"` // yyyy-mm-dd from the search card
	PostedAgo      string `json:"posted_ago,omitempty"`  // e.g. "1 week ago"
	Applicants     string `json:"applicants,omitempty"`  // e.g. "Over 200 applicants"
	Level          string `json:"level,omitempty"`       // "Seniority level", e.g. "Mid-Senior level"
	EmploymentType string `json:"employment_type,omitempty"`
	JobFunction    string `json:"job_function,omitempty"`
	Industries     string `json:"industries,omitempty"`
}

type Skills struct {
	All []string `json:"all,omitempty"`
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// DetailURL builds the job posting URL for a job ID.
func (c *Client) DetailURL(id string) string {
	return c.rebase(strings.Replace(URL_JOB_DETAIL, "{job_id}", url.PathEscape(id), 1))
}

// FetchJobDetail downloads and parses one job posting.
func (c *Client) FetchJobDetail(ctx context.Context, id string) (*JobDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("linkedin: job id is required")
	}
	detailURL := c.DetailURL(id)
	body, err := c.get(ctx, detailURL, c.baseURL+"/jobs/search")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, id)
		}
		return nil, fmt.Errorf("linkedin: parse %s: %w", detailURL, err)
	}
	detail.ID = id
	detail.URL = JobURL(id)
	return detail, nil
}

// FetchJobDetails fetches the pages of jobs one by one, through the client's rate limiter,
// with a longer pause every batchEvery pages. onDetail (optional) is called after each success,
// so callers can persist progress. Jobs that are gone or fail to parse are reported as failures
// and skipped; a block or ctx cancellation stops the run and is returned as the error together
// with everything fetched so far.
func (c *Client) FetchJobDetails(ctx context.Context, jobs []Job, onDetail func(JobDetail)) ([]JobDetail, []DetailFailure, error) {
	var (
		details  []JobDetail
		failures []DetailFailure
	)
	for i, job := range jobs {
		if i > 0 && c.batchEvery > 0 && i%c.batchEvery == 0 {
			pause := c.batchPause + randDuration(c.batchPause/3)
			slog.Info("linkedin: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.ID)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("linkedin: skip job detail", "id", job.ID, "err", err)
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

// ParseJobDetail parses the fragment returned by URL_JOB_DETAIL. A page without the
// top card (removed job, or an empty 200 answer) is reported as ErrNotFound.
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	top := doc.Find(".top-card-layout").First()
	if top.Length() == 0 {
		top = doc.Find(".topcard").First()
	}
	if top.Length() == 0 {
		return nil, ErrNotFound
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	d.Title = htmltext.Clean(top.Find(".top-card-layout__title, .topcard__title").First().Text())
	org := top.Find("a.topcard__org-name-link").First()
	if org.Length() == 0 {
		org = top.Find(".topcard__flavor").First()
	}
	d.Company = htmltext.Clean(org.Text())
	d.CompanyURL = cleanURL(org.AttrOr("href", ""))
	d.Overview.Location = htmltext.Clean(top.Find(".topcard__flavor--bullet").First().Text())
	d.Overview.PostedAgo = htmltext.Clean(top.Find(".posted-time-ago__text").First().Text())
	d.Overview.Applicants = htmltext.Clean(top.Find(".num-applicants__caption").First().Text())
	d.Overview.Salary = htmltext.Clean(doc.Find(".compensation__salary").First().Text())

	d.Description = htmltext.FromSelection(doc.Find(".show-more-less-html__markup").First())
	if d.Description == "" {
		d.Description = htmltext.FromSelection(doc.Find(".description__text").First())
	}

	doc.Find(".description__job-criteria-item").Each(func(_ int, li *goquery.Selection) {
		label := htmltext.Clean(li.Find(".description__job-criteria-subheader").First().Text())
		value := htmltext.Clean(li.Find(".description__job-criteria-text").First().Text())
		if label == "" || value == "" {
			return
		}
		if d.Criteria == nil {
			d.Criteria = map[string]string{}
		}
		d.Criteria[label] = value
		switch strings.ToLower(label) {
		case "seniority level", "cấp bậc":
			d.Overview.Level = value
		case "employment type", "loại hình công việc", "hình thức làm việc":
			d.Overview.EmploymentType = value
		case "job function", "chức năng công việc":
			d.Overview.JobFunction = value
		case "industries", "ngành":
			d.Overview.Industries = value
		}
	})

	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

// mergeJob fills in what only the search card has (exact posting date, logo-side fields).
func (d *JobDetail) mergeJob(j Job) {
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
	if o.Location == "" {
		o.Location = j.Location
	}
	o.DatePosted = j.PostedAt
	if o.PostedAgo == "" {
		o.PostedAgo = j.PostedAgo
	}
}
