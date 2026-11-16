package vietnamworks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

// URL_DETAIL is the job info API (GET); {jobId} is the numeric job ID.
var URL_DETAIL = `https://ms.vietnamworks.com/api-gateway/v1.0/tracking/job-info?jobId={jobId}`

// JobDetail is the full content of a VietnamWorks job, as plain text ready for AI matching.
// It follows topcv.JobDetail so both sources can be matched the same way.
type JobDetail struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	URL           string            `json:"url"`
	Title         string            `json:"title"`
	Company       string            `json:"company"`
	CompanyID     int64             `json:"company_id,omitempty"`
	Overview      Overview          `json:"overview"`
	Description   string            `json:"description"`
	Requirements  string            `json:"requirements"`
	Benefits      string            `json:"benefits"`
	Skills        Skills            `json:"skills"`
	BenefitTags   map[string]string `json:"benefit_tags,omitempty"` // e.g. "Chăm sóc sức khoẻ": "Bảo hiểm sức khoẻ"
	WorkLocations []string          `json:"work_locations,omitempty"`
	ScrapedAt     time.Time         `json:"scraped_at"`
}

// Overview is the summary of a job (salary, location, level, ...).
type Overview struct {
	Salary           string   `json:"salary"` // as displayed, e.g. "18tr-30tr ₫/tháng"
	SalaryMin        *float64 `json:"salary_min,omitempty"`
	SalaryMax        *float64 `json:"salary_max,omitempty"`
	SalaryCurrency   string   `json:"salary_currency,omitempty"`
	Location         string   `json:"location"`
	Experience       string   `json:"experience"`
	ExperienceMonths *int     `json:"experience_months,omitempty"`
	Deadline         string   `json:"deadline"` // yyyy-mm-dd
	ValidThrough     string   `json:"valid_through,omitempty"`
	DatePosted       string   `json:"date_posted,omitempty"`
	Level            string   `json:"level,omitempty"`
	Openings         string   `json:"openings,omitempty"`
	Industry         string   `json:"industry,omitempty"`
	JobFunction      string   `json:"job_function,omitempty"`
}

type Skills struct {
	All []string `json:"all,omitempty"` // VietnamWorks does not split required/preferred skills
}

// DetailFailure records a job whose detail could not be fetched or parsed.
type DetailFailure struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// DetailURL builds the job info API URL for a job ID.
func (c *Client) DetailURL(jobID string) string {
	return c.rebase(strings.Replace(URL_DETAIL, "{jobId}", url.QueryEscape(jobID), 1))
}

// FetchJobDetail downloads and parses the job info of one job. It only has what the
// job info API returns; FetchJobDetails also merges the search data (skills, benefits, salary).
func (c *Client) FetchJobDetail(ctx context.Context, jobID string) (*JobDetail, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("vietnamworks: job id is required")
	}
	detailURL := c.DetailURL(jobID)
	body, err := c.do(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return nil, err
	}
	detail, err := ParseJobDetail(body)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", err, jobID)
		}
		return nil, fmt.Errorf("vietnamworks: parse job %s: %w", jobID, err)
	}
	if detail.ID == "" {
		detail.ID = jobID
	}
	return detail, nil
}

// FetchJobDetails fetches the details of jobs one by one, through the client's rate limiter,
// with a longer pause every batchEvery jobs. onDetail (optional) is called after each success,
// so callers can persist progress. Jobs that are gone or fail to parse are reported as failures
// and skipped; a 403 or ctx cancellation stops the run and is returned as the error together
// with everything fetched so far.
func (c *Client) FetchJobDetails(ctx context.Context, jobs []Job, onDetail func(JobDetail)) ([]JobDetail, []DetailFailure, error) {
	var (
		details  []JobDetail
		failures []DetailFailure
	)
	for i, job := range jobs {
		if i > 0 && c.batchEvery > 0 && i%c.batchEvery == 0 {
			pause := c.batchPause + randDuration(c.batchPause/3)
			slog.Info("vietnamworks: batch pause", "done", i, "total", len(jobs), "pause", pause.Round(time.Second))
			if err := sleep(ctx, pause); err != nil {
				return details, failures, err
			}
		}

		detail, err := c.FetchJobDetail(ctx, job.ID)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
				return details, failures, err
			}
			slog.Warn("vietnamworks: skip job detail", "id", job.ID, "err", err)
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

// --- job info API ---------------------------------------------------------------

type detailResponse struct {
	Status  string    `json:"status"`
	Message string    `json:"message"`
	Data    apiDetail `json:"data"`
}

type apiDetail struct {
	JobID          string `json:"job_id"`
	JobTitle       string `json:"job_title"`
	JobLevelName   string `json:"job_level_name"`
	JobFunction    string `json:"job_function_name"`
	JobLocationCty string `json:"job_location_city"`
	CompanyID      int64  `json:"company_id"`
	CompanyName    string `json:"company_name"`
	Industry       []struct {
		Name string `json:"name"`
	} `json:"company_industry"`
	IsSalaryHidden bool   `json:"is_salary_hidden"`
	SalaryRange    string `json:"salary_range"` // "min-max", "0-0" when hidden
	JobRequirement string `json:"job_requirement"`
	JobDescription string `json:"job_description"`
	YearsRequired  *int   `json:"number_of_years_of_experience_required"`
	Headcount      int    `json:"headcount"`
	ExpiredDate    string `json:"job_expired_date"`
}

// ParseJobDetail parses the JSON returned by URL_DETAIL. The API answers 200 with an
// empty job for unknown IDs, which is reported as ErrNotFound.
func ParseJobDetail(body []byte) (*JobDetail, error) {
	var resp detailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "" && resp.Status != "success" {
		return nil, fmt.Errorf("api error %s: %s", resp.Status, resp.Message)
	}
	a := resp.Data
	if strings.TrimSpace(a.JobTitle) == "" && strings.TrimSpace(a.JobDescription) == "" {
		return nil, ErrNotFound
	}

	d := &JobDetail{
		ID:           a.JobID,
		Source:       Source,
		Title:        htmltext.Clean(a.JobTitle),
		Company:      htmltext.Clean(a.CompanyName),
		CompanyID:    a.CompanyID,
		Description:  htmltext.FromHTML(a.JobDescription),
		Requirements: htmltext.FromHTML(a.JobRequirement),
		ScrapedAt:    time.Now().UTC(),
	}
	o := &d.Overview
	o.Level = a.JobLevelName
	o.JobFunction = a.JobFunction
	o.Location = a.JobLocationCty
	o.Deadline = a.ExpiredDate
	if a.Headcount > 0 {
		o.Openings = strconv.Itoa(a.Headcount)
	}
	var industries []string
	for _, ind := range a.Industry {
		industries = appendUnique(industries, htmltext.Clean(ind.Name))
	}
	o.Industry = strings.Join(industries, ", ")
	if a.YearsRequired != nil {
		o.setExperience(*a.YearsRequired)
	}
	if !a.IsSalaryHidden {
		o.SalaryMin, o.SalaryMax = parseSalaryRange(a.SalaryRange)
	}
	return d, nil
}

// mergeJob fills in what only the search API has (skills, benefits, salary text,
// addresses) and prefers its Vietnamese labels over the job info API's English ones.
func (d *JobDetail) mergeJob(j Job) {
	if j.ID != "" {
		d.ID = j.ID
	}
	d.URL = j.URL
	if j.Title != "" {
		d.Title = j.Title
	}
	if d.Company == "" {
		d.Company = j.Company
	}
	if d.CompanyID == 0 {
		d.CompanyID = j.CompanyID
	}

	o := &d.Overview
	o.Salary = j.Salary
	if j.SalaryMin != nil || j.SalaryMax != nil {
		o.SalaryMin, o.SalaryMax = j.SalaryMin, j.SalaryMax
	}
	o.SalaryCurrency = j.SalaryCurrency
	if j.Location != "" {
		o.Location = j.Location
	}
	if j.Level != "" {
		o.Level = j.Level
	}
	if j.JobFunction != "" {
		o.JobFunction = j.JobFunction
	}
	if len(j.Industries) > 0 {
		o.Industry = strings.Join(j.Industries, ", ")
	}
	if o.Experience == "" && j.ExperienceYears > 0 {
		o.setExperience(j.ExperienceYears)
	}
	if o.Openings == "" && j.Openings > 0 {
		o.Openings = strconv.Itoa(j.Openings)
	}
	o.DatePosted = j.PostedAt
	o.ValidThrough = j.ExpiredAt

	d.Skills.All = j.Skills
	d.WorkLocations = j.Addresses
	var lines []string
	for _, b := range j.Benefits {
		d.BenefitTags = setTag(d.BenefitTags, b.Name, b.Value)
		lines = append(lines, "- "+strings.ReplaceAll(b.Value, "\n", " "))
	}
	d.Benefits = strings.Join(lines, "\n")
}

func (o *Overview) setExperience(years int) {
	if years <= 0 {
		o.Experience = "Không yêu cầu kinh nghiệm"
		return
	}
	months := years * 12
	o.Experience = fmt.Sprintf("%d năm", years)
	o.ExperienceMonths = &months
}

// parseSalaryRange reads "min-max" (e.g. "18000000-30000000"); zeros mean unknown.
func parseSalaryRange(s string) (minV, maxV *float64) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return nil, nil
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(lo), 64); err == nil {
		minV = positive(v)
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(hi), 64); err == nil {
		maxV = positive(v)
	}
	return minV, maxV
}

func setTag(m map[string]string, k, v string) map[string]string {
	if k == "" || v == "" {
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	if old, ok := m[k]; ok {
		v = old + "; " + v
	}
	m[k] = v
	return m
}
