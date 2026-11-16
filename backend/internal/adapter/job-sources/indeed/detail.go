package indeed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// URL_DETAIL_JOB opens a job in the search page's side pane; {job_key} is the job key (jk).
// /viewjob?jk= itself answers 401 to anonymous scrapers, but the side pane embeds the
// same job data in window._initialData.
var URL_DETAIL_JOB = `https://vn.indeed.com/jobs?q=&l={location}&vjk={job_key}`

// JobDetail is the content of an Indeed job, as plain text ready for AI matching.
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
	OtherInfo     map[string]string `json:"other_info,omitempty"` // "Label: value" lines, e.g. "Hạn nộp": "2026-10-28"
	OtherSections map[string]string `json:"other_sections,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job.
type Overview struct {
	Salary         string     `json:"salary"` // as displayed, e.g. "20.000.000 đ / năm"
	SalaryMin      *float64   `json:"salary_min,omitempty"`
	SalaryMax      *float64   `json:"salary_max,omitempty"`
	SalaryCurrency string     `json:"salary_currency,omitempty"`
	SalaryUnit     string     `json:"salary_unit,omitempty"` // MONTHLY, YEARLY, ... (Indeed's extraction is not always right)
	Location       string     `json:"location"`
	WorkType       string     `json:"work_type,omitempty"`
	PostedAt       *time.Time `json:"posted_at,omitempty"`
	Age            string     `json:"age,omitempty"`           // relative, e.g. "1 ngày trước"
	OriginSource   string     `json:"origin_source,omitempty"` // site Indeed aggregated the job from, e.g. "ITviec"
}

// DetailFailure records a job whose page could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

var errNoJobContent = errors.New("no job content found on page")

// DetailURL builds the URL that opens a job key in the search page's side pane (see URL_DETAIL_JOB).
func (c *Client) DetailURL(jobKey string) string {
	return c.rebase(strings.NewReplacer(
		"{location}", url.QueryEscape(cityHaNoi),
		"{job_key}", url.QueryEscape(jobKey),
	).Replace(URL_DETAIL_JOB))
}

var jobKeyRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// jobKeyOf returns the job key of a bare key or of a URL carrying it in ?jk= or ?vjk=.
func jobKeyOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if jobKeyRe.MatchString(ref) {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	for _, k := range []string{"jk", "vjk"} {
		if v := u.Query().Get(k); jobKeyRe.MatchString(v) {
			return v
		}
	}
	return ""
}

// FetchJobDetail downloads and parses one job. ref is a job key, or a job URL with ?jk= or ?vjk=.
func (c *Client) FetchJobDetail(ctx context.Context, ref string) (*JobDetail, error) {
	jk := jobKeyOf(ref)
	if jk == "" {
		return nil, fmt.Errorf("indeed: no job key in %q", ref)
	}
	pageURL := c.DetailURL(jk)

	body, err := c.get(ctx, pageURL, c.baseURL+"/")
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(strings.NewReader(body))
	switch {
	case errors.Is(err, errNoJobContent):
		// Indeed answers 200 with an empty side pane for expired or unknown jobs.
		return nil, fmt.Errorf("%w: %s", ErrNotFound, pageURL)
	case err != nil:
		return nil, fmt.Errorf("indeed: parse %s: %w", pageURL, err)
	case detail.ID != jk:
		return nil, fmt.Errorf("%w: %s (pane shows job %s)", ErrNotFound, pageURL, detail.ID)
	}
	return detail, nil
}

// FetchJobDetails fetches the jobs one by one, through the client's rate limiter,
// with a longer pause every batchEvery pages. onDetail (optional) is called after each success,
// so callers can persist progress. Jobs that are gone or fail to parse are reported as failures and
// skipped; a bot challenge or ctx cancellation stops the run and is returned as the error
// together with everything fetched so far.
func (c *Client) FetchJobDetails(ctx context.Context, jobs []Job, onDetail func(JobDetail)) ([]JobDetail, []DetailFailure, error) {
	var (
		details  []JobDetail
		failures []DetailFailure
	)
	for i, job := range jobs {
		if i > 0 && c.batchEvery > 0 && i%c.batchEvery == 0 {
			pause := c.batchPause + randDuration(c.batchPause/3)
			slog.Info("indeed: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		ref := job.ID
		if ref == "" {
			ref = job.URL
		}
		detail, err := c.FetchJobDetail(ctx, ref)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("indeed: skip job detail", "id", job.ID, "err", err)
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

// viewJobBody is the part of the side pane's view job response the adapter uses.
type viewJobBody struct {
	JobKey              string `json:"jobKey"`
	JobTitle            string `json:"jobTitle"`
	JobLocation         string `json:"jobLocation"`
	JobInfoWrapperModel struct {
		JobInfoModel struct {
			Header struct {
				CompanyName         string `json:"companyName"`
				CompanyOverviewLink string `json:"companyOverviewLink"`
				FormattedLocation   string `json:"formattedLocation"`
				JobTitle            string `json:"jobTitle"`
			} `json:"jobInfoHeaderModel"`
			SanitizedJobDescription string `json:"sanitizedJobDescription"`
		} `json:"jobInfoModel"`
	} `json:"jobInfoWrapperModel"`
	HiringInsightsModel struct {
		Age string `json:"age"`
	} `json:"hiringInsightsModel"`
	JobMetadataFooterModel struct {
		Age    string `json:"age"`
		Source string `json:"source"`
	} `json:"jobMetadataFooterModel"`
	SalaryInfoModel *struct {
		SalaryCurrency string  `json:"salaryCurrency"`
		SalaryMin      float64 `json:"salaryMin"`
		SalaryMax      float64 `json:"salaryMax"`
		SalaryText     string  `json:"salaryText"`
		SalaryType     string  `json:"salaryType"`
	} `json:"salaryInfoModel"`
}

// ParseJobDetail parses the job opened in the side pane of an Indeed search page (see URL_DETAIL_JOB).
func ParseJobDetail(r io.Reader) (*JobDetail, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var init initialData
	if err := decodeScriptVar(string(b), initialDataVar, &init); err != nil {
		return nil, err
	}
	if init.AutoOpenTwoPaneViewjobResponse == nil || init.AutoOpenTwoPaneViewjobResponse.Body == nil {
		return nil, errNoJobContent
	}
	v := init.AutoOpenTwoPaneViewjobResponse.Body
	info := v.JobInfoWrapperModel.JobInfoModel
	if v.JobKey == "" || (v.JobTitle == "" && info.SanitizedJobDescription == "") {
		return nil, errNoJobContent
	}

	d := &JobDetail{
		ID:         v.JobKey,
		Source:     Source,
		URL:        ViewJobURL(v.JobKey),
		Title:      htmltext.Clean(v.JobTitle),
		Company:    htmltext.Clean(info.Header.CompanyName),
		CompanyURL: absURL(info.Header.CompanyOverviewLink),
		ScrapedAt:  time.Now().UTC(),
	}
	if d.Title == "" {
		d.Title = htmltext.Clean(info.Header.JobTitle)
	}
	o := &d.Overview
	o.Location = htmltext.Clean(info.Header.FormattedLocation)
	if o.Location == "" {
		o.Location = htmltext.Clean(v.JobLocation)
	}
	o.Age = htmltext.Clean(v.JobMetadataFooterModel.Age)
	if o.Age == "" {
		o.Age = htmltext.Clean(v.HiringInsightsModel.Age)
	}
	o.OriginSource = htmltext.Clean(v.JobMetadataFooterModel.Source)
	if s := v.SalaryInfoModel; s != nil {
		o.Salary = htmltext.Clean(s.SalaryText)
		o.SalaryMin = positive(s.SalaryMin)
		o.SalaryMax = positive(s.SalaryMax)
		o.SalaryCurrency = s.SalaryCurrency
		o.SalaryUnit = s.SalaryType
	}
	parseDescription(info.SanitizedJobDescription, d)
	return d, nil
}

// parseDescription splits the free-form description into sections by its heading lines.
// Indeed relays jobs from many sites, so headings come as <h2>, as "<b>Label:</b>" paragraphs
// or as bare text ("REQUIREMENTS"); "Label: value" lines go to OtherInfo. When no known
// section is found, the whole text becomes the description.
func parseDescription(fragment string, d *JobDetail) {
	text := descriptionText(fragment)
	if text == "" {
		return
	}

	sections := map[string]*strings.Builder{}
	var order []string
	kind, name := sectionDescription, ""
	known := false // a description/requirements/benefits heading was seen
	add := func(line string) {
		key := kind
		if kind == sectionOther {
			key = name
		}
		b, ok := sections[key]
		if !ok {
			b = &strings.Builder{}
			sections[key] = b
			order = append(order, key)
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}

	for _, line := range strings.Split(text, "\n") {
		if heading, ok := headingOf(line); ok {
			kind, name = classify(heading), heading
			known = known || kind != sectionOther
			// A second heading of the same kind ("Điểm cộng" after "Yêu cầu") stays visible in it.
			if b := sections[kind]; kind != sectionOther && b != nil && b.Len() > 0 {
				add(heading + ":")
			}
			continue
		}
		if label, value, ok := labelValue(line); ok {
			d.OtherInfo = setTag(d.OtherInfo, label, value)
			continue
		}
		add(line)
	}

	if !known {
		d.Description = text
		return
	}
	get := func(key string) string {
		if b := sections[key]; b != nil {
			return b.String()
		}
		return ""
	}
	d.Description = get(sectionDescription)
	d.Requirements = get(sectionRequirements)
	d.Benefits = get(sectionBenefits)
	for _, key := range order {
		switch key {
		case sectionDescription, sectionRequirements, sectionBenefits:
		default:
			d.OtherSections = setTag(d.OtherSections, key, get(key))
		}
	}
}

// descriptionText renders the description as plain text, ending every <h1>-<h6>
// with ":" so headings look the same as "<b>Label:</b>" ones.
func descriptionText(fragment string) string {
	if strings.TrimSpace(fragment) == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + fragment + "</div>"))
	if err != nil {
		return htmltext.FromHTML(fragment)
	}
	doc.Find("h1, h2, h3, h4, h5, h6").Each(func(_ int, h *goquery.Selection) {
		if t := htmltext.Clean(h.Text()); t != "" && !strings.HasSuffix(t, ":") {
			h.AppendHtml(":")
		}
	})
	return htmltext.FromSelection(doc.Find("body > div").First())
}

const (
	sectionDescription  = "description"
	sectionRequirements = "requirements"
	sectionBenefits     = "benefits"
	sectionOther        = ""
)

// Keywords are matched against the lower-cased heading, benefits first, then requirements
// ("Yêu cầu công việc" is a requirement, not a description).
var sectionKeywords = []struct {
	kind     string
	keywords []string
}{
	{sectionBenefits, []string{"quyền lợi", "phúc lợi", "đãi ngộ", "chế độ", "benefit", "perk", "what we offer", "love working"}},
	{sectionRequirements, []string{"yêu cầu", "kỹ năng", "kinh nghiệm", "điểm cộng", "ưu tiên", "requirement", "qualification", "skill", "experience", "nice to have"}},
	{sectionDescription, []string{"mô tả", "nhiệm vụ", "trách nhiệm", "công việc", "description", "responsibilit", "your role", "what you will do", "duties"}},
}

func classify(heading string) string {
	h := strings.ToLower(heading)
	for _, s := range sectionKeywords {
		for _, k := range s.keywords {
			if strings.Contains(h, k) {
				return s.kind
			}
		}
	}
	return sectionOther
}

// headingOf reports whether a line is a section heading: a short line ending with ":"
// ("Quyền lợi được hưởng:"), or a few words naming a known section ("REQUIREMENTS").
func headingOf(line string) (string, bool) {
	if strings.HasPrefix(line, "- ") {
		return "", false
	}
	if h, ok := strings.CutSuffix(line, ":"); ok && utf8.RuneCountInString(line) <= 80 {
		h = strings.TrimSpace(h)
		return h, h != ""
	}
	if !strings.Contains(line, ":") && len(strings.Fields(line)) <= 4 && classify(line) != sectionOther {
		return line, true
	}
	return "", false
}

// labelValue splits a "Label: value" line such as "Hạn nộp: 2026-10-28".
func labelValue(line string) (label, value string, ok bool) {
	if strings.HasPrefix(line, "- ") {
		return "", "", false
	}
	label, value, ok = strings.Cut(line, ":")
	label, value = strings.TrimSpace(label), strings.TrimSpace(value)
	if !ok || label == "" || value == "" || utf8.RuneCountInString(label) > 40 ||
		strings.HasPrefix(label, "(") || strings.HasPrefix(value, "//") {
		return "", "", false
	}
	return label, value, true
}

// mergeJob keeps the search ID and URL and fills what the side pane did not show.
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
	if o.WorkType == "" {
		o.WorkType = j.WorkType
	}
	if o.PostedAt == nil {
		o.PostedAt = j.PostedAt
	}
}

func positive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
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
