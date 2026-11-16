package jobsgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// URL_DETAIL_JOB is the job page URL template; {job-name} is the slug ending with the
// job number, e.g. "lap-trinh-vien-java-29391229498".
var URL_DETAIL_JOB = `https://jobsgo.vn/viec-lam/{job-name}.html`

// JobDetail is the content of a JobsGO job page, as plain text ready for AI matching.
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
	WorkLocations []string          `json:"work_locations,omitempty"`
	WorkingTimes  []string          `json:"working_times,omitempty"`
	OtherInfo     map[string]string `json:"other_info,omitempty"` // e.g. "Yêu cầu ngôn ngữ": "Tiếng Anh"
	OtherSections map[string]string `json:"other_sections,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job page plus its JSON-LD metadata.
type Overview struct {
	Salary           string   `json:"salary"` // as displayed, e.g. "20 - 26 triệu VNĐ"
	SalaryMin        *float64 `json:"salary_min,omitempty"`
	SalaryMax        *float64 `json:"salary_max,omitempty"`
	SalaryCurrency   string   `json:"salary_currency,omitempty"`
	SalaryUnit       string   `json:"salary_unit,omitempty"` // MONTH, YEAR, ...
	Location         string   `json:"location"`
	Experience       string   `json:"experience"`
	ExperienceMonths *int     `json:"experience_months,omitempty"`
	Deadline         string   `json:"deadline"` // dd/mm/yyyy as displayed
	ValidThrough     string   `json:"valid_through,omitempty"`
	DatePosted       string   `json:"date_posted,omitempty"` // JSON-LD datetime, or dd/mm/yyyy when the page has no JSON-LD
	Level            string   `json:"level,omitempty"`
	Education        string   `json:"education,omitempty"`
	Openings         string   `json:"openings,omitempty"`
	WorkType         string   `json:"work_type,omitempty"`
	EmploymentType   string   `json:"employment_type,omitempty"`
	Industry         string   `json:"industry,omitempty"`
	CompanyField     string   `json:"company_field,omitempty"` // employer's "Lĩnh vực"
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// DetailURL builds the job page URL for a job slug (see URL_DETAIL_JOB).
func (c *Client) DetailURL(slug string) string {
	return c.rebase(strings.Replace(URL_DETAIL_JOB, "{job-name}", url.PathEscape(slug), 1))
}

// FetchJobDetail downloads and parses one job page. jobURL is a full job URL as found
// in the search results, or just its slug.
func (c *Client) FetchJobDetail(ctx context.Context, jobURL string) (*JobDetail, error) {
	jobURL = strings.TrimSpace(jobURL)
	if jobURL == "" {
		return nil, errors.New("jobsgo: job url is required")
	}
	if !strings.Contains(jobURL, "://") {
		jobURL = c.DetailURL(strings.TrimSuffix(jobURL, ".html"))
	}
	jobURL = absURL(jobURL)

	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jobsgo: parse %s: %w", jobURL, err)
	}
	detail.URL = jobURL
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
			slog.Info("jobsgo: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("jobsgo: skip job detail", "id", job.ID, "err", err)
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

// ParseJobDetail parses the HTML of a JobsGO job page. Structured fields come from the
// page's JSON-LD JobPosting; sections and the displayed values come from the HTML,
// falling back to the JSON-LD description when the layout is unknown.
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

	if t := htmltext.Clean(doc.Find("h1.job-title").First().Text()); t != "" {
		d.Title = t
	}
	d.ID = strings.TrimSpace(doc.Find("[data-jid]").First().AttrOr("data-jid", ""))
	parseLabels(doc, d)
	parseSections(doc, d)
	parsePlaces(doc, d)

	if d.Description == "" && d.Requirements == "" && ld != nil {
		// The JSON-LD description is already plain text (all sections, "\r\n"-separated).
		d.Description = strings.TrimSpace(strings.ReplaceAll(ld.Description, "\r", ""))
		d.Benefits = strings.TrimSpace(ld.JobBenefits)
	}
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

// parseLabels reads the "Label:" / value pairs spread over the page
// (<span class="text-muted">Mức lương:</span> <strong>20 - 26 triệu VNĐ</strong>).
func parseLabels(doc *goquery.Document, d *JobDetail) {
	o := &d.Overview
	doc.Find(".text-muted").Each(func(_ int, label *goquery.Selection) {
		name, ok := strings.CutSuffix(htmltext.Clean(label.Text()), ":")
		value := label.Next()
		if !ok || name == "" || value.Length() == 0 {
			return
		}
		v := htmltext.Clean(value.Text())
		if v == "" {
			return
		}
		switch name {
		case "Mức lương":
			o.Salary = v
		case "Địa điểm":
			o.Location = v
		case "Kinh nghiệm":
			o.Experience = v
		case "Bằng cấp":
			o.Education = v
		case "Hạn nộp hồ sơ":
			// "18/11/2026 (Còn 29 ngày)"
			o.Deadline, _, _ = strings.Cut(v, " (")
		case "Loại hình":
			o.WorkType = v
		case "Cấp bậc":
			o.Level = v
		case "Số lượng tuyển":
			o.Openings = v
		case "Ngành nghề":
			o.Industry = v
		case "Lĩnh vực":
			o.CompanyField = v
		case "Ngày đăng tuyển":
			if o.DatePosted == "" {
				o.DatePosted = v
			}
		case "Thời gian làm việc":
			d.WorkingTimes = appendUnique(d.WorkingTimes, v)
		case "Địa điểm làm việc":
			// Read item by item in parsePlaces.
		case "Địa chỉ":
			// The employer card's address, not the work place.
			d.OtherInfo = setTag(d.OtherInfo, "Địa chỉ công ty", v)
		default:
			if _, dup := d.OtherInfo[name]; !dup {
				d.OtherInfo = setTag(d.OtherInfo, name, v)
			}
		}
	})
}

func parseSections(doc *goquery.Document, d *JobDetail) {
	doc.Find(".job-detail-card h3.section-title").Each(func(_ int, h *goquery.Selection) {
		heading := strings.TrimSuffix(htmltext.Clean(h.Text()), ":")
		body := htmltext.FromSelection(h.Next())
		switch {
		case strings.HasPrefix(heading, "Mô tả"):
			d.Description = body
		case strings.HasPrefix(heading, "Yêu cầu"):
			d.Requirements = body
		case strings.HasPrefix(heading, "Quyền lợi"):
			d.Benefits = body
		case heading != "" && body != "":
			d.OtherSections = setTag(d.OtherSections, heading, body)
		}
	})
}

var placeNumberRe = regexp.MustCompile(`^\d+\.\s*`)

// parsePlaces reads the detailed work addresses ("1. Lê Văn Lương, Hà Nội"), keeping the
// JSON-LD addresses when the page lists none.
func parsePlaces(doc *goquery.Document, d *JobDetail) {
	var places []string
	doc.Find("#places .location-extra > span:first-child").Each(func(_ int, s *goquery.Selection) {
		places = appendUnique(places, placeNumberRe.ReplaceAllString(htmltext.Clean(s.Text()), ""))
	})
	if len(places) > 0 {
		d.WorkLocations = places
	}
}

// mergeJob keeps the search ID and URL and fills what the page did not show.
func (d *JobDetail) mergeJob(j Job) {
	if j.ID != "" {
		d.ID = j.ID
	}
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
	if o.Location == "" {
		o.Location = j.Location
	}
	if o.Experience == "" {
		o.Experience = j.Experience
	}
	if o.WorkType == "" {
		o.WorkType = j.WorkType
	}
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
	JobBenefits        string          `json:"jobBenefits"`
	ExperienceRequired json.RawMessage `json:"experienceRequirements"`
	HiringOrganization struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"hiringOrganization"`
	BaseSalary struct {
		Currency string   `json:"currency"`
		MinValue ldNumber `json:"minValue"`
		MaxValue ldNumber `json:"maxValue"`
		UnitText string   `json:"unitText"`
	} `json:"baseSalary"`
	JobLocation []struct {
		Address struct {
			StreetAddress string `json:"streetAddress"`
		} `json:"address"`
	} `json:"jobLocation"`
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
	d.CompanyURL = p.HiringOrganization.URL
	o := &d.Overview
	o.DatePosted = p.DatePosted
	o.ValidThrough = p.ValidThrough
	o.EmploymentType = string(p.EmploymentType)
	o.Industry = string(p.Industry)
	o.SalaryCurrency = p.BaseSalary.Currency
	o.SalaryUnit = p.BaseSalary.UnitText
	o.SalaryMin = p.BaseSalary.MinValue.ptr()
	o.SalaryMax = p.BaseSalary.MaxValue.ptr()
	var exp struct {
		Months ldNumber `json:"monthsOfExperience"`
	}
	if json.Unmarshal(p.ExperienceRequired, &exp) == nil {
		if m := exp.Months.ptr(); m != nil {
			months := int(*m)
			o.ExperienceMonths = &months
		}
	}
	for _, l := range p.JobLocation {
		d.WorkLocations = appendUnique(d.WorkLocations, htmltext.Clean(l.Address.StreetAddress))
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

// ldNumber accepts a JSON number or a numeric string; zero and anything else count as absent.
type ldNumber struct {
	v  float64
	ok bool
}

func (n *ldNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
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
