package vieclam24h

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "vieclam24h"

// vnZone is the time zone of the API's dates (unix seconds; deadlines end at 23:59:59 here).
var vnZone = time.FixedZone("ICT", 7*60*60)

// Job is one job of the search API, decoded into plain text ready for AI matching.
// The API has no "job description" or "benefits" in its list; Requirements holds the
// posting's requirement section.
type Job struct {
	ID            string    `json:"id"` // numeric, e.g. "200975010"
	Source        string    `json:"source"`
	URL           string    `json:"url"` // https://vieclam24h.vn/it-phan-mem/java-backend-developer-c8p210000160id200975010.html
	Title         string    `json:"title"`
	Company       string    `json:"company"`
	CompanyLogo   string    `json:"company_logo,omitempty"`
	Salary        string    `json:"salary"`               // e.g. "10 - 17 triệu", "Thỏa thuận"
	SalaryMin     int64     `json:"salary_min,omitempty"` // VND per month
	SalaryMax     int64     `json:"salary_max,omitempty"`
	Location      string    `json:"location"`                 // province names, e.g. "Hà Nội, Hưng Yên"
	WorkLocations []string  `json:"work_locations,omitempty"` // "87 - 89 Khuất Duy Tiến, Hà Nội"
	Experience    string    `json:"experience,omitempty"`     // e.g. "2 năm", "Chưa có kinh nghiệm"
	Level         string    `json:"level,omitempty"`          // e.g. "Nhân viên"
	Degree        string    `json:"degree,omitempty"`         // e.g. "Đại học"
	WorkingMethod string    `json:"working_method,omitempty"` // e.g. "Toàn thời gian cố định"
	Gender        string    `json:"gender,omitempty"`
	Vacancies     int       `json:"vacancies,omitempty"`
	Occupations   []string  `json:"occupations,omitempty"` // e.g. "IT Phần mềm"
	Tags          []string  `json:"tags,omitempty"`        // the site's smart tags (broad categories, not skills)
	Requirements  string    `json:"requirements"`
	PostedAt      string    `json:"posted_at,omitempty"` // RFC 3339
	Deadline      string    `json:"deadline,omitempty"`  // RFC 3339, last moment to apply
	ScrapedAt     time.Time `json:"scraped_at"`
}

// SearchResult is one search page.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	TotalJobs  int    `json:"total_jobs"`
	Jobs       []Job  `json:"jobs"`
}

// apiJob is the part of an API job item used here. The API is loosely typed (a date
// can be false, a number a string), so numbers are read with flexInt.
type apiJob struct {
	ID                   flexInt   `json:"id"`
	Title                string    `json:"title"`
	TitleSlug            string    `json:"title_slug"`
	OccupationIDsMain    []flexInt `json:"occupation_ids_main"`
	ProvinceIDs          []flexInt `json:"province_ids"`
	Places               string    `json:"places"` // a JSON array encoded as a string
	LevelRequirement     flexInt   `json:"level_requirement"`
	ExperienceRange      flexInt   `json:"experience_range"`
	DegreeRequirement    flexInt   `json:"degree_requirement"`
	WorkingMethod        flexInt   `json:"working_method"`
	Gender               flexInt   `json:"gender"`
	VacancyQuantity      flexInt   `json:"vacancy_quantity"`
	SalaryMin            flexInt   `json:"salary_min"`
	SalaryMax            flexInt   `json:"salary_max"`
	JobRequirementHTML   string    `json:"job_requirement_html"`
	OtherRequirementHTML string    `json:"other_requirement_html"`
	ResumeApplyExpired   flexInt   `json:"resume_apply_expired"`
	ApprovedAt           flexInt   `json:"approved_at"`
	CreatedAt            flexInt   `json:"created_at"`
	EmployerInfo         struct {
		Name string  `json:"name"`
		Logo *string `json:"logo"`
	} `json:"employer_info"`
	SmartTags []struct {
		Name string `json:"name"`
	} `json:"smart_tags"`
}

type apiPlace struct {
	ProvinceID flexInt `json:"province_id"`
	Address    string  `json:"address"`
}

// flexInt reads a JSON number, a numeric string, or false/true/null/"" (as 0).
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch string(b) {
	case "null", "false", "true", `""`:
		*f = 0
		return nil
	}
	b = bytes.Trim(b, `"`)
	if n, err := strconv.ParseInt(string(b), 10, 64); err == nil {
		*f = flexInt(n)
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("not a number: %s", b)
	}
	*f = flexInt(v)
	return nil
}

func ints(list []flexInt) []int {
	out := make([]int, len(list))
	for i, v := range list {
		out[i] = int(v)
	}
	return out
}

// ParseJobList decodes a get-job-list answer.
func ParseJobList(body []byte) (*SearchResult, error) {
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.Code != 200 {
		return nil, fmt.Errorf("api error %d: %s", env.Code, env.Msg)
	}
	var data struct {
		Items      []json.RawMessage `json:"items"`
		TotalPages flexInt           `json:"total_pages"`
		TotalItems flexInt           `json:"total_items"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, err
	}
	if data.Items == nil && data.TotalItems > 0 {
		return nil, errors.New("no items in answer")
	}

	now := time.Now().UTC()
	result := &SearchResult{TotalPages: int(data.TotalPages), TotalJobs: int(data.TotalItems), Jobs: make([]Job, 0, len(data.Items))}
	for i, raw := range data.Items {
		// One odd item must not cost the whole page.
		var it apiJob
		if err := json.Unmarshal(raw, &it); err != nil {
			slog.Warn("vieclam24h: skip job item", "index", i, "err", err)
			continue
		}
		if it.ID == 0 {
			continue
		}
		j := it.toJob()
		j.ScrapedAt = now
		result.Jobs = append(result.Jobs, j)
	}
	return result, nil
}

func (it apiJob) toJob() Job {
	t := lookups()
	j := Job{
		ID:            strconv.FormatInt(int64(it.ID), 10),
		Source:        Source,
		URL:           it.url(),
		Title:         htmltext.Clean(it.Title),
		Company:       htmltext.Clean(it.EmployerInfo.Name),
		Salary:        salaryText(int64(it.SalaryMin), int64(it.SalaryMax)),
		SalaryMin:     int64(it.SalaryMin),
		SalaryMax:     int64(it.SalaryMax),
		Experience:    codeName(t.ExperienceRange, int(it.ExperienceRange)),
		Level:         codeName(t.LevelRequirement, int(it.LevelRequirement)),
		Degree:        codeName(t.DegreeRequirement, int(it.DegreeRequirement)),
		WorkingMethod: codeName(t.WorkingMethod, int(it.WorkingMethod)),
		Gender:        codeName(t.Gender, int(it.Gender)),
		Vacancies:     int(it.VacancyQuantity),
		Requirements:  joinText(htmltext.FromHTML(it.JobRequirementHTML), htmltext.FromHTML(it.OtherRequirementHTML)),
		PostedAt:      unixTime(firstNonZero(int64(it.ApprovedAt), int64(it.CreatedAt))),
		Deadline:      unixTime(int64(it.ResumeApplyExpired)),
	}
	if it.EmployerInfo.Logo != nil {
		j.CompanyLogo = *it.EmployerInfo.Logo
	}

	var cities []string
	for _, id := range ints(it.ProvinceIDs) {
		if p, ok := ProvinceByID(id); ok {
			cities = appendUnique(cities, p.Name)
		}
	}
	j.Location = strings.Join(cities, ", ")
	var places []apiPlace
	if json.Unmarshal([]byte(it.Places), &places) == nil {
		for _, pl := range places {
			addr := htmltext.Clean(pl.Address)
			if p, ok := ProvinceByID(int(pl.ProvinceID)); ok && addr != "" {
				addr += ", " + p.Name
			}
			j.WorkLocations = appendUnique(j.WorkLocations, addr)
		}
	}
	for _, id := range ints(it.OccupationIDsMain) {
		j.Occupations = appendUnique(j.Occupations, occupationName(id))
	}
	for _, tag := range it.SmartTags {
		j.Tags = appendUnique(j.Tags, htmltext.Clean(tag.Name))
	}
	return j
}

// url builds the job page link the way the site does:
// /{occupation slug}/{title slug}-c{occupation}p{province}id{job}.html. The site redirects
// any link with the right "-c…p…id….html" tail to the canonical one.
func (it apiJob) url() string {
	occ, prov := 0, 0
	if len(it.OccupationIDsMain) > 0 {
		occ = int(it.OccupationIDsMain[0])
	}
	if len(it.ProvinceIDs) > 0 {
		prov = int(it.ProvinceIDs[0])
	}
	prefix := occupationSlug(occ)
	if prefix == "" {
		prefix = "viec-lam"
	}
	slug := it.TitleSlug
	if slug == "" {
		slug = htmltext.Slugify(it.Title)
	}
	return fmt.Sprintf("%s/%s/%s-c%dp%did%d.html", siteURL, prefix, slug, occ, prov, int64(it.ID))
}

// salaryText renders a monthly salary in VND as the other sources show it:
// "10 - 17 triệu", "Từ 10 triệu", "Tới 17 triệu" or "Thỏa thuận".
func salaryText(minVND, maxVND int64) string {
	m := func(v int64) string { return strconv.FormatFloat(float64(v)/1e6, 'f', -1, 64) }
	switch {
	case minVND > 0 && maxVND > 0 && minVND != maxVND:
		return m(minVND) + " - " + m(maxVND) + " triệu"
	case minVND > 0 && maxVND > 0:
		return m(minVND) + " triệu"
	case minVND > 0:
		return "Từ " + m(minVND) + " triệu"
	case maxVND > 0:
		return "Tới " + m(maxVND) + " triệu"
	}
	return "Thỏa thuận"
}

func unixTime(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(vnZone).Format(time.RFC3339)
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func joinText(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
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
