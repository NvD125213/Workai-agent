package careerlink

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

// JobDetail is the content of a CareerLink job, as plain text ready for AI matching.
// It follows topcv.JobDetail so all sources can be matched the same way.
type JobDetail struct {
	ID           string            `json:"id"`
	Source       string            `json:"source"`
	URL          string            `json:"url"`
	Title        string            `json:"title"`
	Company      string            `json:"company"`
	Overview     Overview          `json:"overview"`
	Description  string            `json:"description"`
	Requirements string            `json:"requirements"` // "Kinh nghiệm / Kỹ năng chi tiết"
	Benefits     string            `json:"benefits"`     // "Phúc lợi"
	Skills       Skills            `json:"skills"`
	Info         map[string]string `json:"info,omitempty"` // the "Mô tả" summary box, as displayed
	ScrapedAt    time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job, normalized from the page and its JSON-LD.
type Overview struct {
	Salary      string `json:"salary"`               // e.g. "7 triệu - 9 triệu", "Thương lượng"
	Location    string `json:"location"`             // e.g. "Ô Chợ Dừa, Hà Nội"
	Experience  string `json:"experience,omitempty"` // e.g. "0 - 1 năm kinh nghiệm"
	Education   string `json:"education,omitempty"`  // e.g. "Đại học"
	WorkType    string `json:"work_type,omitempty"`  // e.g. "Nhân viên toàn thời gian"
	CareerLevel string `json:"career_level,omitempty"`
	Industry    string `json:"industry,omitempty"`
	DatePosted  string `json:"date_posted,omitempty"` // yyyy-mm-dd
	Deadline    string `json:"deadline,omitempty"`    // yyyy-mm-dd
	// Expired: the page says the posting is closed. The AI service also drops such jobs
	// by Deadline.
	Expired bool `json:"expired,omitempty"`
}

type Skills struct {
	All []string `json:"all,omitempty"` // the "Từ khóa" tags under the job
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// FetchJobDetail downloads and parses one job page (the title link of a search card).
func (c *Client) FetchJobDetail(ctx context.Context, jobURL string) (*JobDetail, error) {
	jobURL = absURL(jobURL)
	if !strings.Contains(jobURL, "/tim-viec-lam/") {
		return nil, fmt.Errorf("careerlink: not a job URL: %q", jobURL)
	}
	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/vieclam/list")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, jobURL)
		}
		return nil, fmt.Errorf("careerlink: parse %s: %w", jobURL, err)
	}
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
			slog.Info("careerlink: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("careerlink: skip job detail", "id", job.ID, "err", err)
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

// ParseJobDetail parses a job page: the visible blocks (overview under the title, the
// sections by id) first, the JSON-LD JobPosting for the rest. A page with neither is
// ErrNotFound (a removed job shows "Rất tiếc, chúng tôi không tìm thấy việc làm…").
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	ld := findJobPosting(doc)
	title := doc.Find("[data-testid='job-detail-title'], h1#job-title").First()
	if title.Length() == 0 && ld == nil {
		return nil, ErrNotFound
	}

	d := &JobDetail{
		Source:    Source,
		Title:     htmltext.Clean(title.Text()),
		Company:   htmltext.Clean(doc.Find("[class*='__companyName']").First().Text()),
		ScrapedAt: time.Now().UTC(),
	}
	parseOverview(doc.Find("[data-testid='job-overview']").First(), d)
	d.Description = sectionText(doc.Find("#section-job-description"))
	d.Benefits = sectionText(doc.Find("#section-job-benefits"))
	d.Requirements = sectionText(doc.Find("#section-job-skills"))
	parseSummary(doc.Find("#section-job-summary"), d)
	doc.Find("#section-job-tags a").Each(func(_ int, a *goquery.Selection) {
		if tag := htmltext.Clean(a.Text()); tag != "" {
			d.Skills.All = append(d.Skills.All, tag)
		}
	})
	if ld != nil {
		ld.fill(d)
	}

	o := &d.Overview
	o.Experience = firstNonEmpty(d.Info["Kinh nghiệm"], o.Experience)
	o.Education = firstNonEmpty(d.Info["Học vấn"], o.Education)
	o.WorkType = firstNonEmpty(d.Info["Loại công việc"], o.WorkType)
	o.CareerLevel = firstNonEmpty(d.Info["Cấp bậc"], o.CareerLevel)
	o.Industry = firstNonEmpty(d.Info["Ngành nghề"], o.Industry)
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

var (
	dmyDashRe = regexp.MustCompile(`(\d{1,2})-(\d{1,2})-(\d{4})`)
	isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
)

// parseOverview reads the rows under the title, told apart by their lucide icon:
//
//	map-pin             Ô Chợ Dừa, Hà Nội
//	circle-dollar-sign  7 triệu - 9 triệu
//	briefcase           0 - 1 năm kinh nghiệm
//	calendar            Ngày đăng tuyển 12-11-2026 | Hết hạn trong: 31 Ngày
func parseOverview(box *goquery.Selection, d *JobDetail) {
	o := &d.Overview
	box.Find("[class*='__overviewRow']").Each(func(_ int, row *goquery.Selection) {
		icon := row.Find("svg").First().AttrOr("class", "")
		text := htmltext.Clean(row.Text())
		lower := strings.ToLower(text)
		switch {
		case strings.Contains(icon, "lucide-map-pin") || row.Find("[data-testid='job-location-wards']").Length() > 0:
			// <a>Ô Chợ Dừa</a>,<a>Hà Nội</a>: wards, then the province.
			var places []string
			row.Find("a").Each(func(_ int, a *goquery.Selection) {
				if p := htmltext.Clean(a.Text()); p != "" {
					places = append(places, p)
				}
			})
			o.Location = firstNonEmpty(strings.Join(places, ", "), text)
		case strings.Contains(icon, "dollar"):
			o.Salary = text
		case strings.Contains(icon, "lucide-calendar") || strings.Contains(lower, "ngày đăng tuyển"):
			if m := dmyDashRe.FindStringSubmatch(text); m != nil {
				day, _ := strconv.Atoi(m[1])
				month, _ := strconv.Atoi(m[2])
				o.DatePosted = fmt.Sprintf("%s-%02d-%02d", m[3], month, day)
			}
			// An open job counts down ("Hết hạn trong: 31 Ngày"); a closed one does not.
			if strings.Contains(lower, "hết hạn") && !strings.Contains(lower, "hết hạn trong") {
				o.Expired = true
			}
		case strings.Contains(icon, "lucide-briefcase") || strings.Contains(lower, "kinh nghiệm"):
			o.Experience = text
		}
	})
}

// parseSummary reads the "Mô tả" box: items of <span>label</span><span>value</span>.
func parseSummary(box *goquery.Selection, d *JobDetail) {
	box.Find("[class*='__summaryItem']").Each(func(_ int, item *goquery.Selection) {
		pair := item.ChildrenFiltered("span").Last().ChildrenFiltered("span")
		if pair.Length() < 2 {
			return
		}
		label := htmltext.Clean(pair.Eq(0).Text())
		value := strings.ReplaceAll(htmltext.Clean(pair.Eq(1).Text()), " ,", ",")
		d.Info = setTag(d.Info, label, value)
	})
}

// sectionText renders a <section> without its <h2> heading.
func sectionText(section *goquery.Selection) string {
	if section.Length() == 0 {
		return ""
	}
	body := section.First().Clone()
	body.ChildrenFiltered("h2").Remove()
	return strings.TrimSpace(htmltext.FromSelection(body))
}

// jobPosting is the part of the page's JSON-LD JobPosting used here.
type jobPosting struct {
	Type                   string `json:"@type"`
	Title                  string `json:"title"`
	Description            string `json:"description"` // description and skills, merged
	DatePosted             string `json:"datePosted"`
	ValidThrough           string `json:"validThrough"` // "2026-12-12T23:59:59.000Z": the last day
	EmploymentType         string `json:"employmentType"`
	Industry               string `json:"industry"`
	ExperienceRequirements string `json:"experienceRequirements"` // "no requirements" or text
	HiringOrganization     struct {
		Name string `json:"name"`
	} `json:"hiringOrganization"`
	JobLocation []struct {
		Address struct {
			Locality string `json:"addressLocality"`
			Region   string `json:"addressRegion"`
		} `json:"address"`
	} `json:"jobLocation"`
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
	if isoDateRe.MatchString(p.DatePosted) {
		o.DatePosted = p.DatePosted[:10]
	}
	// validThrough is the end of the last day to apply, written as UTC but meant as local
	// time; its date part is the deadline.
	if isoDateRe.MatchString(p.ValidThrough) {
		o.Deadline = p.ValidThrough[:10]
	}
	d.Title = firstNonEmpty(d.Title, htmltext.Clean(p.Title))
	d.Company = firstNonEmpty(d.Company, htmltext.Clean(p.HiringOrganization.Name))
	if d.Description == "" {
		d.Description = htmltext.FromHTML(p.Description)
	}
	if o.Location == "" {
		var places []string
		for _, l := range p.JobLocation {
			if place := htmltext.Clean(firstNonEmpty(l.Address.Region, l.Address.Locality)); place != "" {
				places = append(places, place)
			}
		}
		o.Location = strings.Join(places, ", ")
	}
	if o.Experience == "" && p.ExperienceRequirements != "" {
		o.Experience = p.ExperienceRequirements
		if strings.EqualFold(p.ExperienceRequirements, "no requirements") {
			o.Experience = "Không yêu cầu kinh nghiệm"
		}
	}
	o.WorkType = firstNonEmpty(o.WorkType, workTypeNames[strings.ToUpper(p.EmploymentType)], p.EmploymentType)
	o.Industry = firstNonEmpty(o.Industry, p.Industry)
}

var workTypeNames = map[string]string{
	"FULL_TIME":  "Toàn thời gian",
	"PART_TIME":  "Bán thời gian",
	"INTERN":     "Thực tập",
	"CONTRACTOR": "Hợp đồng",
	"TEMPORARY":  "Thời vụ",
	"OTHER":      "Khác",
}

// mergeJob fills in what only the search card has.
func (d *JobDetail) mergeJob(j Job) {
	d.Title = firstNonEmpty(d.Title, j.Title)
	d.Company = firstNonEmpty(d.Company, j.Company)
	o := &d.Overview
	o.Salary = firstNonEmpty(o.Salary, j.Salary)
	o.Location = firstNonEmpty(o.Location, j.Location)
	o.CareerLevel = firstNonEmpty(o.CareerLevel, j.CareerLevel)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
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
