package onetwothreejob

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

// JobDetail is the content of a 123job job, as plain text ready for AI matching.
// It follows topcv.JobDetail so all sources can be matched the same way.
type JobDetail struct {
	ID           string            `json:"id"`
	Source       string            `json:"source"`
	URL          string            `json:"url"`
	Title        string            `json:"title"`
	Company      string            `json:"company"`
	Overview     Overview          `json:"overview"`
	Description  string            `json:"description"`
	Requirements string            `json:"requirements"`
	Benefits     string            `json:"benefits"`
	Skills       Skills            `json:"skills"`
	Info         map[string]string `json:"info,omitempty"` // "Yêu cầu ứng viên" and "Thông tin chung" rows, as displayed
	ScrapedAt    time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job, normalized from the page and its JSON-LD.
type Overview struct {
	Salary     string `json:"salary"`               // e.g. "10 - 15 triệu"; "" when not given
	Location   string `json:"location"`             // e.g. "Tây Hồ, Hà Nội"
	Experience string `json:"experience,omitempty"` // e.g. "2 năm", "6 tháng"
	Education  string `json:"education,omitempty"`  // e.g. "Đại học", "Không yêu cầu"
	WorkType   string `json:"work_type,omitempty"`  // e.g. "Toàn thời gian"
	Industry   string `json:"industry,omitempty"`   // e.g. "IT phần mềm"
	Openings   string `json:"openings,omitempty"`
	DatePosted string `json:"date_posted,omitempty"` // yyyy-mm-dd
	Deadline   string `json:"deadline,omitempty"`    // yyyy-mm-dd
	// Expired: the page marks the posting closed ("Hết hạn"). Search results still list
	// such jobs; the AI service drops them by Deadline.
	Expired bool `json:"expired,omitempty"`
}

type Skills struct {
	All []string `json:"all,omitempty"` // 123job shows no skill tags; kept for symmetry
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
	if !strings.Contains(jobURL, "/viec-lam/") {
		return nil, fmt.Errorf("123job: not a job URL: %q", jobURL)
	}
	body, err := c.get(ctx, c.rebase(jobURL), c.baseURL+"/tuyen-dung")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, jobURL)
		}
		return nil, fmt.Errorf("123job: parse %s: %w", jobURL, err)
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
			slog.Info("123job: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.URL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("123job: skip job detail", "id", job.ID, "err", err)
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

// ParseJobDetail parses a job page: the visible sections (each an <h2> followed by its
// content) first, the JSON-LD JobPosting for the rest. A page with neither is ErrNotFound.
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	ld := findJobPosting(doc)
	h1 := doc.Find("h1").First()
	if h1.Length() == 0 && ld == nil {
		return nil, ErrNotFound
	}

	d := &JobDetail{Source: Source, ScrapedAt: time.Now().UTC()}
	parseHead(h1, d)
	parseSections(doc, d)
	if ld != nil {
		ld.fill(d)
	}
	o := &d.Overview
	o.Experience = experienceText(firstNonEmpty(d.Info["Kinh nghiệm"], o.Experience))
	o.Education = educationText(firstNonEmpty(d.Info["Trình độ học vấn"], o.Education))
	o.WorkType = workTypeText(firstNonEmpty(d.Info["Hình thức làm việc"], o.WorkType))
	o.Industry = firstNonEmpty(d.Info["Ngành nghề"], o.Industry)
	if v := d.Info["Số lượng tuyển"]; v != "" && v != "__" {
		o.Openings = v
	}
	if d.Title == "" && d.Description == "" {
		return nil, errors.New("no job content found on page")
	}
	return d, nil
}

// parseHead reads the block under the title: salary and location, then the deadline.
//
//	<h1>Backend Developer</h1>
//	<div><span>10 - 15 triệu</span><span>Hà Nội</span></div>
//	<div><span>Đăng: 1 tuần trước</span>·<span id="deadlineLabel">Hạn nộp: 10/12/2026</span></div>
func parseHead(h1 *goquery.Selection, d *JobDetail) {
	// A closed posting has a badge in its title: <h1>Senior BA <span>Hết hạn</span></h1>.
	title := h1.Clone()
	badges := title.ChildrenFiltered("span")
	badges.Each(func(_ int, b *goquery.Selection) {
		if strings.EqualFold(htmltext.Clean(b.Text()), "Hết hạn") {
			d.Overview.Expired = true
		}
	})
	badges.Remove()
	d.Title = htmltext.Clean(title.Text())
	meta := h1.NextAllFiltered("div").First().ChildrenFiltered("span")
	d.Overview.Salary = salaryText(meta.Eq(0).Text())
	d.Overview.Location = htmltext.Clean(meta.Eq(1).Text())
	if label := htmltext.Clean(h1.Parent().Find("#deadlineLabel").Text()); label != "" {
		d.Overview.Deadline = isoDate(label)
	}
}

// parseSections reads each "<h2>Heading</h2><div>…</div>" block of the job.
func parseSections(doc *goquery.Document, d *JobDetail) {
	doc.Find("h2").Each(func(_ int, h2 *goquery.Selection) {
		heading := htmltext.Clean(h2.Text())
		body := h2.Next()
		switch strings.ToLower(heading) {
		case "mô tả công việc":
			d.Description = sectionText(body, heading)
		case "yêu cầu công việc":
			d.Requirements = sectionText(body, heading)
		case "quyền lợi":
			d.Benefits = sectionText(body, heading)
		case "yêu cầu ứng viên", "thông tin chung":
			// Rows of <div><p>Label</p><p>Value</p></div>.
			body.Find("div").Each(func(_ int, row *goquery.Selection) {
				ps := row.ChildrenFiltered("p")
				if ps.Length() == 2 {
					d.Info = setTag(d.Info, htmltext.Clean(ps.Eq(0).Text()), htmltext.Clean(ps.Eq(1).Text()))
				}
			})
		}
	})
}

// sectionText renders a section, without the heading the employer often repeats as its
// first line, in full or shortened ("<p>Mô tả công việc</p>…", "<p>Yêu cầu</p>…" under
// "Yêu cầu công việc").
func sectionText(body *goquery.Selection, heading string) string {
	text := htmltext.FromSelection(body)
	first, rest, _ := strings.Cut(text, "\n")
	first = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(first), ":"))
	if first != "" && strings.HasPrefix(strings.ToLower(heading), first) {
		text = rest
	}
	return strings.TrimSpace(text)
}

// jobPosting is the part of the page's JSON-LD JobPosting used here.
type jobPosting struct {
	Type                   string          `json:"@type"`
	Title                  string          `json:"title"`
	Description            string          `json:"description"`
	JobBenefits            string          `json:"jobBenefits"`
	DatePosted             string          `json:"datePosted"`
	ValidThrough           string          `json:"validThrough"`
	EmploymentType         string          `json:"employmentType"`
	Industry               string          `json:"industry"`
	ExperienceRequirements json.RawMessage `json:"experienceRequirements"` // "OccupationalExperienceRequirements, 24" or an object
	EducationRequirements  json.RawMessage `json:"educationRequirements"`
	TotalJobOpenings       json.RawMessage `json:"totalJobOpenings"`
	HiringOrganization     struct {
		Name string `json:"name"`
	} `json:"hiringOrganization"`
	JobLocation struct {
		Address struct {
			Locality string `json:"addressLocality"`
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
	o.DatePosted = p.DatePosted
	if o.Deadline == "" {
		o.Deadline = p.ValidThrough
	}
	if d.Title == "" {
		d.Title = htmltext.Clean(p.Title)
	}
	d.Company = htmltext.Clean(p.HiringOrganization.Name)
	if d.Description == "" {
		d.Description = sectionText(htmlSelection(p.Description), "Mô tả công việc")
	}
	if d.Benefits == "" {
		d.Benefits = sectionText(htmlSelection(p.JobBenefits), "Quyền lợi")
	}
	if o.Location == "" {
		o.Location = htmltext.Clean(p.JobLocation.Address.Locality)
	}
	o.Experience = firstNonEmpty(o.Experience, rawText(p.ExperienceRequirements))
	o.Education = firstNonEmpty(o.Education, rawText(p.EducationRequirements))
	o.WorkType = firstNonEmpty(o.WorkType, p.EmploymentType)
	o.Industry = firstNonEmpty(o.Industry, p.Industry)
}

// htmlSelection parses an HTML fragment from the JSON-LD.
func htmlSelection(fragment string) *goquery.Selection {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + fragment + "</div>"))
	if err != nil {
		return &goquery.Selection{}
	}
	return doc.Find("body > div").First()
}

// rawText reads a JSON-LD value that may be a string, a number or an object with a
// "description" or "monthsOfExperience".
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	var obj struct {
		Description string  `json:"description"`
		Months      float64 `json:"monthsOfExperience"`
		Credential  string  `json:"credentialCategory"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		switch {
		case obj.Months > 0:
			return "OccupationalExperienceRequirements, " + strconv.FormatFloat(obj.Months, 'f', -1, 64)
		case obj.Credential != "":
			return obj.Credential
		}
		return obj.Description
	}
	return ""
}

var schemaValueRe = regexp.MustCompile(`^(?:OccupationalExperienceRequirements|EducationalOccupationalCredential),\s*(.*)$`)

// experienceText fixes the site's raw schema.org value, shown as is on the page
// ("OccupationalExperienceRequirements, 24" = 24 months), into text the AI service reads:
// "2 năm", "6 tháng", "Không yêu cầu kinh nghiệm". Other text is kept.
func experienceText(s string) string {
	s = htmltext.Clean(s)
	m := schemaValueRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	months, err := strconv.ParseFloat(strings.TrimSpace(m[1]), 64)
	if err != nil {
		return strings.TrimSpace(m[1])
	}
	switch {
	case months <= 0:
		return "Không yêu cầu kinh nghiệm"
	case int(months)%12 == 0:
		return strconv.Itoa(int(months)/12) + " năm"
	default:
		return strconv.FormatFloat(months, 'f', -1, 64) + " tháng"
	}
}

var educationNames = map[string]string{
	"bachelor degree":          "Đại học",
	"associate degree":         "Cao đẳng",
	"high school":              "Trung học",
	"postgraduate":             "Trên đại học",
	"master degree":            "Thạc sĩ",
	"doctoral degree":          "Tiến sĩ",
	"professional certificate": "Chứng chỉ",
}

// educationText turns "EducationalOccupationalCredential, bachelor degree" into "Đại học".
func educationText(s string) string {
	s = htmltext.Clean(s)
	if m := schemaValueRe.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	if vi, ok := educationNames[strings.ToLower(s)]; ok {
		return vi
	}
	return s
}

var workTypeNames = map[string]string{
	"FULL_TIME":  "Toàn thời gian",
	"PART_TIME":  "Bán thời gian",
	"INTERN":     "Thực tập",
	"CONTRACTOR": "Hợp đồng",
	"TEMPORARY":  "Thời vụ",
	"OTHER":      "Khác",
}

func workTypeText(s string) string {
	s = htmltext.Clean(s)
	if vi, ok := workTypeNames[strings.ToUpper(s)]; ok {
		return vi
	}
	return s
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
