package joboko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// JobDetail is the content of a Joboko job, as plain text ready for AI matching.
// It follows topcv.JobDetail so all sources can be matched the same way.
type JobDetail struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	URL           string            `json:"url"`
	Title         string            `json:"title"`
	Company       string            `json:"company"`
	CompanyURL    string            `json:"company_url,omitempty"`
	Overview      Overview          `json:"overview"`
	Description   string            `json:"description"`
	Requirements  string            `json:"requirements"`
	Benefits      string            `json:"benefits"`
	Skills        Skills            `json:"skills"`
	WorkLocations []string          `json:"work_locations,omitempty"` // full addresses ("Nơi làm việc")
	GeneralInfo   map[string]string `json:"general_info,omitempty"`   // "Thông tin chung", e.g. "Thu nhập": "12 - 18 triệu VND"
	OtherSections map[string]string `json:"other_sections,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary at the top of a job page plus its JSON-LD metadata.
type Overview struct {
	Salary         string `json:"salary"`               // e.g. "12 - 18 triệu VND"
	Location       string `json:"location"`             // provinces, e.g. "Hà Nội"
	Experience     string `json:"experience,omitempty"` // e.g. "2 - 5 năm kinh nghiệm"
	Deadline       string `json:"deadline,omitempty"`   // yyyy-mm-dd ("Hạn nộp hồ sơ"), as displayed
	DatePosted     string `json:"date_posted,omitempty"`
	ValidThrough   string `json:"valid_through,omitempty"` // RFC 3339 (JSON-LD); often 00:00 of the deadline day
	EmploymentType string `json:"employment_type,omitempty"`
	Industries     string `json:"industries,omitempty"`
}

type Skills struct {
	All []string `json:"all,omitempty"` // Joboko shows no skill tags; kept for symmetry
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// FetchJobDetail downloads and parses one job page, given the href of the job title
// on a search page (absolute or relative, "/viec-lam-php-developer-xvi6834332").
func (c *Client) FetchJobDetail(ctx context.Context, jobURL string) (*JobDetail, error) {
	jobURL = absURL(jobURL)
	id := jobID(jobURL)
	if id == "" {
		return nil, fmt.Errorf("joboko: not a job URL: %q", jobURL)
	}
	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/jobs")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, jobURL)
		}
		return nil, fmt.Errorf("joboko: parse %s: %w", jobURL, err)
	}
	detail.ID = id
	detail.URL = jobURL
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
			slog.Info("joboko: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("joboko: skip job detail", "id", job.ID, "err", err)
			failures = append(failures, DetailFailure{ID: job.ID, URL: job.URL, Error: err.Error()})
			continue
		}
		detail.ID = job.ID
		detail.mergeJob(job)
		details = append(details, *detail)
		if onDetail != nil {
			onDetail(*detail)
		}
	}
	return details, failures, nil
}

// ParseJobDetail parses a job page. The HTML (what visitors see) wins; the JSON-LD
// JobPosting gives the company and fills what the layout did not give. A page with
// neither is reported as ErrNotFound.
//
// Note: the page's ".name-company" belongs to a personal-data consent popup and names
// the same company on every job, so it is never used.
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	ld := findJobPosting(doc)
	head := doc.Find(".int-job-detail-head").First()
	if head.Length() == 0 && ld == nil {
		return nil, ErrNotFound
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	parseHead(head, d)
	parseContent(doc.Find(".int-job-detail-wysiwyg").First(), d)
	if ld != nil {
		ld.fill(d)
	}
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

func parseHead(h *goquery.Selection, d *JobDetail) {
	d.Title = htmltext.Clean(h.Find("h1").First().Text())
	o := &d.Overview

	var cities []string
	h.Find(".int-job-detail-location a, .int-job-detail-location dd").Each(func(_ int, s *goquery.Selection) {
		if s.Is("dd") && s.Find("a").Length() > 0 {
			return
		}
		cities = appendUnique(cities, htmltext.Clean(s.Text()))
	})
	o.Location = strings.Join(cities, ", ")

	salary := h.Find(".int-job-detail-salary").First()
	o.Salary = htmltext.Clean(salary.AttrOr("title", ""))
	if o.Salary == "" {
		o.Salary = htmltext.Clean(salary.Find("dd").Text())
	}

	// The experience row has no class, only its icon. Some employers put something else
	// there ("Toàn thời gian"), so only text about experience is kept.
	h.Find(".int-job-detail-entry dl").Each(func(_ int, dl *goquery.Selection) {
		if !strings.Contains(dl.Find("dt img").AttrOr("src", ""), "kinh-nghiem") {
			return
		}
		v := htmltext.Clean(dl.Find("dd").Text())
		if low := strings.ToLower(v); strings.Contains(low, "kinh nghiệm") || strings.ContainsAny(low, "0123456789") {
			o.Experience = v
		}
	})
	o.Deadline = isoDate(h.Find(".int-job-detail-deadline b").First().Text())
}

// parseContent reads the <h3> sections of the job body. Each heading is followed by a
// div whose class says what it is (job-desc, job-requirement, ...).
func parseContent(c *goquery.Selection, d *JobDetail) {
	// Some postings are one block without headings.
	if full := c.Find(".job-desc-full").First(); full.Length() > 0 {
		d.Description = htmltext.FromSelection(full)
	}
	c.Find("h3").Each(func(_ int, h3 *goquery.Selection) {
		heading := htmltext.Clean(h3.Text())
		body := h3.Next()
		if heading == "" || !body.Is("div") {
			return
		}
		text := htmltext.FromSelection(body)
		switch {
		case body.HasClass("job-desc"):
			d.Description = text
		case body.HasClass("job-requirement"):
			d.Requirements = text
		case body.HasClass("job-benefit"):
			d.Benefits = text
		case body.HasClass("job-base-infos"):
			// "Thu nhập: 12 - 18 triệu VND", ...
			body.Find("li").Each(func(_ int, li *goquery.Selection) {
				if k, v, ok := strings.Cut(htmltext.Clean(li.Text()), ":"); ok {
					d.GeneralInfo = setTag(d.GeneralInfo, strings.TrimSpace(k), strings.TrimSpace(v))
				}
			})
		case body.HasClass("job-work-places"):
			body.Find("li").Each(func(_ int, li *goquery.Selection) {
				d.WorkLocations = appendUnique(d.WorkLocations, htmltext.Clean(li.Text()))
			})
		default:
			d.OtherSections = setTag(d.OtherSections, heading, text)
		}
	})
}

// jobPosting is the part of the page's JSON-LD JobPosting used here.
type jobPosting struct {
	Type               string          `json:"@type"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	DatePosted         string          `json:"datePosted"`
	ValidThrough       string          `json:"validThrough"`
	EmploymentType     json.RawMessage `json:"employmentType"` // string or list
	Industry           string          `json:"industry"`
	HiringOrganization struct {
		Name   string `json:"name"`
		SameAs string `json:"sameAs"`
	} `json:"hiringOrganization"`
	BaseSalary struct {
		Value struct {
			Value string `json:"value"`
		} `json:"value"`
	} `json:"baseSalary"`
	JobLocation json.RawMessage `json:"jobLocation"` // object or list
}

func findJobPosting(doc *goquery.Document) *jobPosting {
	var found *jobPosting
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		var p jobPosting
		if json.Unmarshal([]byte(s.Text()), &p) == nil && p.Type == "JobPosting" {
			found = &p
			return false
		}
		return true
	})
	return found
}

func (p *jobPosting) fill(d *JobDetail) {
	o := &d.Overview
	o.DatePosted = p.DatePosted
	o.ValidThrough = p.ValidThrough
	o.Industries = p.Industry
	var types []string
	if json.Unmarshal(p.EmploymentType, &types) != nil {
		var one string
		if json.Unmarshal(p.EmploymentType, &one) == nil && one != "" {
			types = []string{one}
		}
	}
	o.EmploymentType = strings.Join(types, ", ")

	d.Company = htmltext.Clean(p.HiringOrganization.Name)
	d.CompanyURL = p.HiringOrganization.SameAs
	if d.Title == "" {
		d.Title = htmltext.Clean(p.Title)
	}
	if d.Description == "" {
		d.Description = htmltext.FromHTML(p.Description)
	}
	if o.Salary == "" {
		o.Salary = htmltext.Clean(p.BaseSalary.Value.Value)
	}
	if o.Location == "" {
		type place struct {
			Address struct {
				Locality string `json:"addressLocality"`
			} `json:"address"`
		}
		var places []place
		if json.Unmarshal(p.JobLocation, &places) != nil {
			var one place
			if json.Unmarshal(p.JobLocation, &one) == nil {
				places = []place{one}
			}
		}
		var cities []string
		for _, pl := range places {
			cities = appendUnique(cities, htmltext.Clean(pl.Address.Locality))
		}
		o.Location = strings.Join(cities, ", ")
	}
}

// mergeJob fills in what only the search card has.
func (d *JobDetail) mergeJob(j Job) {
	if d.Title == "" {
		d.Title = j.Title
	}
	if d.Company == "" {
		d.Company = j.Company
	}
	o := &d.Overview
	if o.Salary == "" {
		o.Salary = j.Salary
	}
	if o.Location == "" {
		o.Location = j.Location
	}
	if o.Deadline == "" {
		o.Deadline = j.Deadline
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
