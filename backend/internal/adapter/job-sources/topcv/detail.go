package topcv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// JobDetail is the content of a TopCV job page, as plain text ready for AI matching.
type JobDetail struct {
	ID              string            `json:"id"`
	Source          string            `json:"source"`
	URL             string            `json:"url"`
	Title           string            `json:"title"`
	Company         string            `json:"company"`
	CompanyURL      string            `json:"company_url,omitempty"`
	Overview        Overview          `json:"overview"`
	Description     string            `json:"description"`
	Requirements    string            `json:"requirements"`
	Benefits        string            `json:"benefits"`
	Income          string            `json:"income,omitempty"` // "Thu nhập" section some postings have
	Skills          Skills            `json:"skills"`
	RequirementTags map[string]string `json:"requirement_tags,omitempty"` // e.g. "Kiến thức ngành": "IT - Phần mềm"
	BenefitTags     map[string]string `json:"benefit_tags,omitempty"`     // e.g. "Phụ cấp": "Ăn trưa"
	WorkLocations   []string          `json:"work_locations,omitempty"`
	WorkingTimes    []string          `json:"working_times,omitempty"`
	HowToApply      string            `json:"how_to_apply,omitempty"`
	OtherSections   map[string]string `json:"other_sections,omitempty"`
	ScrapedAt       time.Time         `json:"scraped_at"`
}

// Overview is the summary block at the top of a job page plus its JSON-LD metadata.
type Overview struct {
	Salary           string   `json:"salary"` // as displayed, e.g. "Tới 25 triệu"
	SalaryMin        *float64 `json:"salary_min,omitempty"`
	SalaryMax        *float64 `json:"salary_max,omitempty"`
	SalaryCurrency   string   `json:"salary_currency,omitempty"`
	SalaryUnit       string   `json:"salary_unit,omitempty"` // MONTH, YEAR, ...
	Location         string   `json:"location"`
	Experience       string   `json:"experience"`
	ExperienceMonths *int     `json:"experience_months,omitempty"`
	Deadline         string   `json:"deadline"` // dd/mm/yyyy as displayed
	ValidThrough     string   `json:"valid_through,omitempty"`
	DatePosted       string   `json:"date_posted,omitempty"`
	Level            string   `json:"level,omitempty"`
	Education        string   `json:"education,omitempty"`
	Openings         string   `json:"openings,omitempty"`
	WorkType         string   `json:"work_type,omitempty"`
	EmploymentType   string   `json:"employment_type,omitempty"`
	Industry         string   `json:"industry,omitempty"`
}

type Skills struct {
	Required  []string `json:"required,omitempty"`
	Preferred []string `json:"preferred,omitempty"`
	All       []string `json:"all,omitempty"` // JSON-LD "skills", useful when the page has no split
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

var jobIDRe = regexp.MustCompile(`(?:/|-j)(\d+)\.html`)

// FetchJobDetail downloads and parses one job page.
func (c *Client) FetchJobDetail(ctx context.Context, jobURL string) (*JobDetail, error) {
	jobURL = cleanJobURL(jobURL)
	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("topcv: parse %s: %w", jobURL, err)
	}
	detail.URL = jobURL
	if m := jobIDRe.FindStringSubmatch(jobURL); m != nil {
		detail.ID = m[1]
	}
	return detail, nil
}

// FetchJobDetails fetches the pages of jobs one by one, through the client's rate limiter,
// with a longer pause every batchEvery pages. onDetail (optional) is called after each success,
// so callers can persist progress. Jobs that 404 or fail to parse are reported as failures and
// skipped; a Cloudflare challenge or ctx cancellation stops the run and is returned as the error
// together with everything fetched so far.
func (c *Client) FetchJobDetails(ctx context.Context, jobs []Job, onDetail func(JobDetail)) ([]JobDetail, []DetailFailure, error) {
	var (
		details  []JobDetail
		failures []DetailFailure
	)
	for i, job := range jobs {
		if i > 0 && c.batchEvery > 0 && i%c.batchEvery == 0 {
			pause := c.batchPause + randDuration(c.batchPause/3)
			slog.Info("topcv: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("topcv: skip job detail", "id", job.ID, "err", err)
			failures = append(failures, DetailFailure{ID: job.ID, URL: job.URL, Error: err.Error()})
			continue
		}
		if job.ID != "" {
			detail.ID = job.ID
		}
		details = append(details, *detail)
		if onDetail != nil {
			onDetail(*detail)
		}
	}
	return details, failures, nil
}

// ParseJobDetail parses the HTML of a TopCV job page. Structured fields come from the
// page's JSON-LD JobPosting; sections come from the HTML, falling back to the JSON-LD
// description when the layout is unknown (e.g. company "brand" pages).
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	ld := findJobPosting(doc)
	if ld != nil {
		ld.applyTo(d)
	}

	parseHeader(doc, d)
	parseGeneralInfo(doc, d)
	parseSections(doc, d)
	parseAddressAndTime(doc, d)

	if d.Description == "" && d.Requirements == "" && ld != nil {
		applyLDSections(ld, d)
	}
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

func parseHeader(doc *goquery.Document, d *JobDetail) {
	title := doc.Find("h1.box-header-job__title").First().Clone()
	title.Find(".icon-verified-employer").Remove()
	if t := htmltext.Clean(title.Text()); t != "" {
		d.Title = t
	}
	if s := htmltext.Clean(doc.Find(".box-header-job__salary--title").First().Text()); s != "" {
		d.Overview.Salary = s
	}
	doc.Find(".box-header-job-list-info__item").Each(func(_ int, s *goquery.Selection) {
		value := htmltext.Clean(s.Find(".list-info__content__desc").Text())
		switch label := htmltext.Clean(s.Find(".list-info__content__title").Text()); {
		case strings.HasPrefix(label, "Địa điểm"):
			d.Overview.Location = value
		case strings.HasPrefix(label, "Kinh nghiệm"):
			d.Overview.Experience = value
		case strings.HasPrefix(label, "Hạn"):
			d.Overview.Deadline = value
		}
	})
	if d.Overview.Deadline == "" {
		d.Overview.Deadline = htmltext.Clean(doc.Find(".box-applied-cv .date").First().Text())
	}
}

func parseGeneralInfo(doc *goquery.Document, d *JobDetail) {
	doc.Find(".box-job-information-general-info-list__item").Each(func(_ int, s *goquery.Selection) {
		value := htmltext.Clean(s.Find(".box-job-information-general-info-list__item--content-desc").Text())
		switch label := htmltext.Clean(s.Find(".box-job-information-general-info-list__item--content-title").Text()); label {
		case "Cấp bậc":
			d.Overview.Level = value
		case "Học vấn":
			d.Overview.Education = value
		case "Số lượng tuyển":
			d.Overview.Openings = value
		case "Hình thức làm việc":
			d.Overview.WorkType = value
		}
	})
}

func parseSections(doc *goquery.Document, d *JobDetail) {
	doc.Find(".box-job-information-detail-item").Each(func(_ int, s *goquery.Selection) {
		heading := htmltext.Clean(s.Find(".box-job-information-detail-item__title--title").First().Text())
		body := htmltext.FromSelection(s.Find(".box-job-information-detail-item__text").First())

		tags := map[string]string{}
		s.Find(".required-tag").Each(func(_ int, t *goquery.Selection) {
			k := htmltext.Clean(t.Find(".required-tag__content--title").Text())
			v := htmltext.Clean(t.Find(".required-tag__content--desc").Text())
			if k != "" && v != "" {
				tags[k] = v
			}
		})

		switch {
		case strings.HasPrefix(heading, "Mô tả"):
			d.Description = body
		case strings.HasPrefix(heading, "Yêu cầu"):
			d.Requirements = body
			for k, v := range tags {
				switch k {
				case "Kỹ năng cần có":
					d.Skills.Required = splitList(v)
				case "Kỹ năng nên có":
					d.Skills.Preferred = splitList(v)
				default:
					d.RequirementTags = setTag(d.RequirementTags, k, v)
				}
			}
		case strings.HasPrefix(heading, "Quyền lợi"):
			d.Benefits = body
			for k, v := range tags {
				d.BenefitTags = setTag(d.BenefitTags, k, v)
			}
		case strings.HasPrefix(heading, "Thu nhập"):
			d.Income = body
		case heading != "" && body != "":
			d.OtherSections = setTag(d.OtherSections, heading, body)
		}
	})
}

func parseAddressAndTime(doc *goquery.Document, d *JobDetail) {
	doc.Find(".box-job-information-address-and-time-list__item").Each(func(_ int, s *goquery.Selection) {
		content := s.Find(".box-job-information-address-and-time-list__item--content")
		var items []string
		content.Find("li").Each(func(_ int, li *goquery.Selection) {
			if v := htmltext.Clean(li.Text()); v != "" {
				items = append(items, v)
			}
		})
		switch label := htmltext.Clean(s.Find(".box-job-information-address-and-time-list__item--title").Text()); {
		case strings.HasPrefix(label, "Địa điểm"):
			d.WorkLocations = items
		case strings.HasPrefix(label, "Thời gian"):
			d.WorkingTimes = items
		case strings.HasPrefix(label, "Cách thức"):
			d.HowToApply = strings.Join(items, "\n")
		}
	})
}

// --- JSON-LD ---------------------------------------------------------------

type ldJobPosting struct {
	Type               string          `json:"@type"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	DatePosted         string          `json:"datePosted"`
	ValidThrough       string          `json:"validThrough"`
	EmploymentType     ldText          `json:"employmentType"`
	Industry           ldText          `json:"industry"`
	Skills             ldText          `json:"skills"`
	JobBenefits        string          `json:"jobBenefits"`
	TotalJobOpenings   ldNumber        `json:"totalJobOpenings"`
	OccupationalCat    ldText          `json:"occupationalCategory"`
	ExperienceRequired json.RawMessage `json:"experienceRequirements"`
	HiringOrganization struct {
		Name   string `json:"name"`
		SameAs string `json:"sameAs"`
	} `json:"hiringOrganization"`
	BaseSalary struct {
		Currency string `json:"currency"`
		Value    struct {
			UnitText string   `json:"unitText"`
			MinValue ldNumber `json:"minValue"`
			MaxValue ldNumber `json:"maxValue"`
			Value    ldNumber `json:"value"`
		} `json:"value"`
	} `json:"baseSalary"`
}

func findJobPosting(doc *goquery.Document) *ldJobPosting {
	var found *ldJobPosting
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		var p ldJobPosting
		if err := json.Unmarshal([]byte(s.Text()), &p); err != nil || p.Type != "JobPosting" {
			return true
		}
		found = &p
		return false
	})
	return found
}

func (p *ldJobPosting) applyTo(d *JobDetail) {
	d.Title = htmltext.Clean(p.Title)
	d.Company = htmltext.Clean(p.HiringOrganization.Name)
	d.CompanyURL = p.HiringOrganization.SameAs
	o := &d.Overview
	o.DatePosted = p.DatePosted
	o.ValidThrough = p.ValidThrough
	o.EmploymentType = string(p.EmploymentType)
	o.Industry = string(p.Industry)
	o.Level = string(p.OccupationalCat)
	o.SalaryCurrency = p.BaseSalary.Currency
	o.SalaryUnit = p.BaseSalary.Value.UnitText
	o.SalaryMin = p.BaseSalary.Value.MinValue.ptr()
	o.SalaryMax = p.BaseSalary.Value.MaxValue.ptr()
	if o.SalaryMin == nil && o.SalaryMax == nil {
		o.SalaryMin = p.BaseSalary.Value.Value.ptr()
		o.SalaryMax = o.SalaryMin
	}
	if n := p.TotalJobOpenings.ptr(); n != nil {
		o.Openings = strconv.Itoa(int(*n))
	}
	var exp struct {
		Months ldNumber `json:"monthsOfExperience"`
	}
	if json.Unmarshal(p.ExperienceRequired, &exp) == nil {
		if m := exp.Months.ptr(); m != nil {
			months := int(*m)
			o.ExperienceMonths = &months
		}
	}
	d.Skills.All = splitList(string(p.Skills))
}

var ldHeadingRe = regexp.MustCompile(`(?i)<h2[^>]*>(.*?)</h2>`)

// applyLDSections splits the JSON-LD description on its <h2> headings
// ("Mô tả công việc", "Yêu cầu ứng viên", ...) when the HTML layout was not recognised.
func applyLDSections(p *ldJobPosting, d *JobDetail) {
	desc := p.Description
	locs := ldHeadingRe.FindAllStringSubmatchIndex(desc, -1)
	if len(locs) == 0 {
		d.Description = htmltext.FromHTML(desc)
	}
	for i, loc := range locs {
		end := len(desc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		heading := htmltext.Clean(htmltext.FromHTML(desc[loc[2]:loc[3]]))
		body := htmltext.FromHTML(desc[loc[1]:end])
		switch {
		case strings.HasPrefix(heading, "Mô tả"):
			d.Description = body
		case strings.HasPrefix(heading, "Yêu cầu"):
			d.Requirements = body
		case strings.HasPrefix(heading, "Quyền lợi"):
			d.Benefits = body
		case strings.HasPrefix(heading, "Thu nhập"):
			d.Income = body
		case heading != "" && body != "":
			d.OtherSections = setTag(d.OtherSections, heading, body)
		}
	}
	if d.Benefits == "" {
		d.Benefits = htmltext.FromHTML(p.JobBenefits)
	}
}

// ldText accepts a JSON string or an array of strings (joined with ", ").
type ldText string

func (t *ldText) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = ldText(s)
		return nil
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		*t = ldText(strings.Join(list, ", "))
	}
	return nil
}

// ldNumber accepts a JSON number or a numeric string; anything else is treated as absent.
type ldNumber struct {
	v  float64
	ok bool
}

func (n *ldNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		n.v, n.ok = v, true
	}
	return nil
}

func (n ldNumber) ptr() *float64 {
	if !n.ok {
		return nil
	}
	v := n.v
	return &v
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = htmltext.Clean(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func setTag(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}
