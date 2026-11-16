package careerviet

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

// JobDetail is the content of a CareerViet job, as plain text ready for AI matching.
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
	WorkLocations []string          `json:"work_locations,omitempty"` // full addresses
	OtherInfo     map[string]string `json:"other_info,omitempty"`     // "Thông tin khác", e.g. "Bằng cấp": "Đại học"
	OtherSections map[string]string `json:"other_sections,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary block at the top of a job page plus its JSON-LD metadata.
type Overview struct {
	Salary       string `json:"salary"`
	Location     string `json:"location"`
	Experience   string `json:"experience,omitempty"` // e.g. "Trên 3 Năm"
	Level        string `json:"level,omitempty"`      // "Cấp bậc", e.g. "Nhân viên"
	WorkType     string `json:"work_type,omitempty"`  // "Hình thức", e.g. "Nhân viên chính thức"
	Industries   string `json:"industries,omitempty"` // "Ngành nghề"
	Education    string `json:"education,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`    // yyyy-mm-dd ("Ngày cập nhật")
	Deadline     string `json:"deadline,omitempty"`      // yyyy-mm-dd ("Hết hạn nộp"), as displayed
	DatePosted   string `json:"date_posted,omitempty"`   // RFC 3339 (JSON-LD)
	ValidThrough string `json:"valid_through,omitempty"` // RFC 3339 (JSON-LD); can differ from Deadline
}

type Skills struct {
	// Keywords is the JSON-LD "skills": search tags chosen by the employer, mostly
	// job-title synonyms ("lập trình viên, developer, php"), so not a skill list.
	Keywords []string `json:"keywords,omitempty"`
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// FetchJobDetail downloads and parses one job page. jobURL is the href of the job title on a
// search page, absolute or relative ("/vi/tim-viec-lam/video-editor.35C8B46A.html").
func (c *Client) FetchJobDetail(ctx context.Context, jobURL string) (*JobDetail, error) {
	jobURL = absURL(jobURL)
	id := jobID(jobURL)
	if id == "" {
		return nil, fmt.Errorf("careerviet: not a job URL: %q", jobURL)
	}
	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/viec-lam/")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, jobURL)
		}
		return nil, fmt.Errorf("careerviet: parse %s: %w", jobURL, err)
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
			slog.Info("careerviet: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("careerviet: skip job detail", "id", job.ID, "err", err)
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
// JobPosting fills what the page layout did not give, since some employers have their
// own page templates. A page with neither is reported as ErrNotFound.
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	ld := findJobPosting(doc)
	header := doc.Find(".job-desc-main").First()
	if header.Length() == 0 && ld == nil {
		return nil, ErrNotFound
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	parseHeader(header, d)
	parseOverview(doc, d)
	parseSections(doc, d)
	if d.Benefits == "" {
		// Employers' own templates put the benefits in a side box (.right-welfares).
		d.Benefits = welfareList(doc.Find(".welfare-list").First())
	}
	if ld != nil {
		ld.fill(d)
	}
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

func parseHeader(h *goquery.Selection, d *JobDetail) {
	d.Title = htmltext.Clean(h.Find("h1").First().Text())
	company := h.Find("a.job-company-name, a.employer").First()
	d.Company = htmltext.Clean(company.Text())
	d.CompanyURL = absURL(company.AttrOr("href", ""))
	d.Overview.Salary = salaryText(h.Find(".meta-salary").First().Text())

	var cities []string
	h.Find(".meta-location-text a").Each(func(_ int, a *goquery.Selection) {
		cities = appendUnique(cities, htmltext.Clean(a.Text()))
	})
	d.Overview.Location = strings.Join(cities, ", ")
	if d.Overview.Location == "" {
		d.Overview.Location = htmltext.Clean(h.Find(".meta-location-text").First().Text())
	}
}

// parseOverview reads the "Hình thức / Kinh nghiệm / Cấp bậc / ... / Hết hạn nộp" summary:
// a grid below the header on the standard page, a "Thông Tin Tuyển Dụng" table on
// employers' own templates.
func parseOverview(doc *goquery.Document, d *JobDetail) {
	doc.Find(".job-overview-item").Each(func(_ int, item *goquery.Selection) {
		setOverview(d, item.Find(".overview-label").First(), item.Find(".overview-value").First())
	})
	doc.Find(".box-info tr").Each(func(_ int, tr *goquery.Selection) {
		setOverview(d, tr.Find("td.name").First(), tr.Find("td.content").First())
	})
}

func setOverview(d *JobDetail, labelSel, valueSel *goquery.Selection) {
	label := labelSel.Clone()
	label.Find("em, i, svg").Remove() // material icon names are text ("layers", "event")
	value := htmltext.Clean(valueSel.Text())
	if value == "" {
		return
	}
	o := &d.Overview
	switch strings.ToLower(htmltext.Clean(label.Text())) {
	case "hình thức":
		o.WorkType = value
	case "kinh nghiệm":
		o.Experience = value
	case "cấp bậc":
		o.Level = value
	case "ngành nghề":
		o.Industries = value
	case "ngày cập nhật":
		o.UpdatedAt = isoDate(value)
	case "hết hạn nộp", "hạn nộp":
		o.Deadline = isoDate(value)
	case "lương":
		if o.Salary == "" {
			o.Salary = salaryText(value)
		}
	case "địa điểm":
		if o.Location == "" {
			var cities []string
			valueSel.Find(".list-workplace a").Each(func(_ int, a *goquery.Selection) {
				cities = appendUnique(cities, htmltext.Clean(a.Text()))
			})
			o.Location = strings.Join(cities, ", ")
		}
	}
}

// parseSections reads the "Mô tả Công việc", "Yêu Cầu Công Việc"... rows. Employers' own
// templates move them out of .job-detail-content, so every .detail-row is read.
func parseSections(doc *goquery.Document, d *JobDetail) {
	doc.Find(".detail-row").Each(func(_ int, row *goquery.Selection) {
		title := row.ChildrenFiltered(".detail-title")
		if title.Length() == 0 {
			return
		}
		heading := htmltext.Clean(title.First().Text())
		body := row.Clone()
		body.ChildrenFiltered(".detail-title").Remove()

		switch lower := strings.ToLower(heading); {
		case lower == "gợi ý hồ sơ", strings.HasPrefix(lower, "job tags"):
			// CareerViet's CV suggestions and the search tags (already in Skills.Keywords)
		case lower == "mô tả công việc":
			d.Description = htmltext.FromSelection(body)
		case lower == "yêu cầu công việc":
			d.Requirements = htmltext.FromSelection(body)
		case lower == "phúc lợi":
			if d.Benefits = welfareList(row.Find(".welfare-list")); d.Benefits == "" {
				d.Benefits = htmltext.FromSelection(body)
			}
		case lower == "thông tin khác":
			// "Bằng cấp: Đại học", "Độ tuổi: Không giới hạn tuổi", "Lương: Cạnh tranh"
			row.Find("li").Each(func(_ int, li *goquery.Selection) {
				k, v, ok := strings.Cut(htmltext.Clean(li.Text()), ":")
				if ok {
					d.OtherInfo = setTag(d.OtherInfo, strings.TrimSpace(k), strings.TrimSpace(v))
				}
			})
			d.Overview.Education = d.OtherInfo["Bằng cấp"]
		case lower == "địa điểm làm việc":
			row.Find(".place-name span").Each(func(_ int, s *goquery.Selection) {
				d.WorkLocations = appendUnique(d.WorkLocations, htmltext.Clean(s.Text()))
			})
		default:
			if text := htmltext.FromSelection(body); heading != "" && text != "" {
				d.OtherSections = setTag(d.OtherSections, heading, text)
			}
		}
	})
}

// welfareList renders the benefit icons list ("Laptop", "Chế độ bảo hiểm"...) as "- " lines.
func welfareList(list *goquery.Selection) string {
	var lines []string
	list.Find("li").Each(func(_ int, li *goquery.Selection) {
		if text := htmltext.Clean(li.Text()); text != "" {
			lines = appendUnique(lines, "- "+text)
		}
	})
	return strings.Join(lines, "\n")
}

// jobPosting is the part of the page's JSON-LD JobPosting used here.
type jobPosting struct {
	Type                   string          `json:"@type"`
	Title                  string          `json:"title"`
	Description            string          `json:"description"`
	DatePosted             string          `json:"datePosted"`
	ValidThrough           string          `json:"validThrough"`
	Skills                 string          `json:"skills"`
	Industry               string          `json:"industry"`
	ExperienceRequirements json.RawMessage `json:"experienceRequirements"`
	HiringOrganization     struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"hiringOrganization"`
	JobLocation json.RawMessage `json:"jobLocation"`
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
	for _, k := range strings.Split(p.Skills, ",") {
		d.Skills.Keywords = appendUnique(d.Skills.Keywords, strings.TrimSpace(k))
	}
	if d.Title == "" {
		d.Title = htmltext.Clean(p.Title)
	}
	if d.Company == "" {
		d.Company = htmltext.Clean(p.HiringOrganization.Name)
		d.CompanyURL = p.HiringOrganization.URL
	}
	if d.Description == "" {
		d.Description = htmltext.FromHTML(p.Description)
	}
	if o.Industries == "" {
		o.Industries = p.Industry
	}
	if o.Experience == "" {
		var exp struct {
			Description string `json:"description"` // "Kinh nghiệm 3 Năm"
		}
		if json.Unmarshal(p.ExperienceRequirements, &exp) == nil {
			o.Experience = strings.TrimSpace(strings.TrimPrefix(exp.Description, "Kinh nghiệm"))
		}
	}
	if o.Location == "" {
		var loc struct {
			Address struct {
				Locality string `json:"addressLocality"`
			} `json:"address"`
		}
		if json.Unmarshal(p.JobLocation, &loc) == nil {
			o.Location = loc.Address.Locality
		}
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
	if o.Deadline == "" {
		o.Deadline = j.Deadline
	}
	if o.UpdatedAt == "" {
		o.UpdatedAt = j.UpdatedAt
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
