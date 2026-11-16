package vietnamworks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"notification-job-mb/internal/adapter/job-sources/htmltext"
)

const Source = "vietnamworks"

// cityHaNoi is VietnamWorks' cityId for Ha Noi.
const cityHaNoi = 24

// Job is a single job posting from a VietnamWorks search result page.
// Unlike TopCV, the search API already returns skills, benefits and the start of
// the description; FetchJobDetails merges them into JobDetail.
type Job struct {
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Title           string    `json:"title"`
	URL             string    `json:"url"`
	Company         string    `json:"company"`
	CompanyID       int64     `json:"company_id,omitempty"`
	CompanyLogo     string    `json:"company_logo,omitempty"`
	Salary          string    `json:"salary"` // as displayed, e.g. "18tr-30tr ₫/tháng", "Thương lượng"
	SalaryMin       *float64  `json:"salary_min,omitempty"`
	SalaryMax       *float64  `json:"salary_max,omitempty"`
	SalaryCurrency  string    `json:"salary_currency,omitempty"`
	Location        string    `json:"location"`
	Addresses       []string  `json:"addresses,omitempty"` // "Hà Nội: Keangnam Landmark 72, ..."
	Level           string    `json:"level,omitempty"`
	Skills          []string  `json:"skills,omitempty"`
	Benefits        []Benefit `json:"benefits,omitempty"`
	Industries      []string  `json:"industries,omitempty"`
	JobFunction     string    `json:"job_function,omitempty"`
	ExperienceYears int       `json:"experience_years,omitempty"`
	Openings        int       `json:"openings,omitempty"`
	PostedAt        string    `json:"posted_at,omitempty"`  // approvedOn, RFC 3339
	ExpiredAt       string    `json:"expired_at,omitempty"` // expiredOn, RFC 3339
	ScrapedAt       time.Time `json:"scraped_at"`
}

type Benefit struct {
	Name  string `json:"name"` // Vietnamese name, e.g. "Chăm sóc sức khoẻ"
	Value string `json:"value"`
}

// SearchResult is one parsed search page.
type SearchResult struct {
	Keyword    string `json:"keyword"`
	SourceURL  string `json:"source_url"`
	Page       int    `json:"page"` // starts at 1
	TotalPages int    `json:"total_pages"`
	TotalJobs  int    `json:"total_jobs"`
	Jobs       []Job  `json:"jobs"`
}

// --- request ------------------------------------------------------------------

type searchFilter struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// PayloadURLJobSearch is the JSON body of URL_JOB_SEARCH.
type PayloadURLJobSearch struct {
	UserID         int            `json:"userId"`
	Query          string         `json:"query"`
	Filter         []searchFilter `json:"filter"`
	Ranges         []any          `json:"ranges"`
	Order          []any          `json:"order"`
	HitsPerPage    int            `json:"hitsPerPage"`
	Page           int            `json:"page"` // starts at 0
	RetrieveFields []string       `json:"retrieveFields"`
	SummaryVersion string         `json:"summaryVersion"`
	Variants       string         `json:"variants"`
	VariantsV2     string         `json:"variants_v2"`
	VariantsV4     string         `json:"variants_v4"`
}

// retrieveFields lists the job fields the search API fills in; any other field comes back empty.
var retrieveFields = []string{
	"address", "benefits", "jobTitle", "salaryMax", "isSalaryVisible", "jobLevelVI", "isShowLogo",
	"salaryMin", "companyLogo", "userId", "jobLevel", "jobLevelId", "jobId", "jobUrl", "companyId",
	"approvedOn", "isAnonymous", "alias", "expiredOn", "industries", "industriesV3", "workingLocations",
	"services", "companyName", "salary", "onlineOn", "onlineOnText", "simpleServices", "visibilityDisplay",
	"isShowLogoInSearch", "priorityOrder", "skills", "profilePublishedSiteMask", "jobDescription",
	"jobRequirement", "prettySalary", "requiredCoverLetter", "languageSelectedVI", "languageSelected",
	"languageSelectedId", "typeWorkingId", "createdOn", "isAdrLiteJob", "applicantSignal",
	"numOfApplications", "numberOfRecruits", "exposureStatus", "isTopPriorityV4", "topPriorityType",
	"salaryCurrency", "jobFunctionsV3", "yearsOfExperience",
}

// newSearchPayload builds the search body for jobs in Ha Noi. page starts at 0.
func newSearchPayload(keyword string, page, hitsPerPage int) PayloadURLJobSearch {
	return PayloadURLJobSearch{
		Query: strings.TrimSpace(keyword),
		Filter: []searchFilter{
			{Field: "workingLocations.cityId", Value: strconv.Itoa(cityHaNoi)},
			{Field: "workingLocations.districtId", Value: fmt.Sprintf(`[{"cityId":%d,"districtId":[-1]}]`, cityHaNoi)},
		},
		Ranges:         []any{},
		Order:          []any{},
		HitsPerPage:    hitsPerPage,
		Page:           page,
		RetrieveFields: retrieveFields,
		Variants:       "test",
		VariantsV2:     "test",
		VariantsV4:     "test-2-allocation-v4",
	}
}

// --- response -----------------------------------------------------------------

type searchResponse struct {
	Meta struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		NbHits  int    `json:"nbHits"`
		Page    int    `json:"page"`
		NbPages int    `json:"nbPages"`
	} `json:"meta"`
	Data []apiJob `json:"data"`
}

type apiJob struct {
	JobID           int64   `json:"jobId"`
	JobTitle        string  `json:"jobTitle"`
	JobURL          string  `json:"jobUrl"`
	Alias           string  `json:"alias"`
	CompanyName     string  `json:"companyName"`
	CompanyID       int64   `json:"companyId"`
	CompanyLogo     string  `json:"companyLogo"`
	ApprovedOn      string  `json:"approvedOn"`
	ExpiredOn       string  `json:"expiredOn"`
	IsSalaryVisible bool    `json:"isSalaryVisible"`
	SalaryMin       float64 `json:"salaryMin"`
	SalaryMax       float64 `json:"salaryMax"`
	SalaryCurrency  string  `json:"salaryCurrency"`
	PrettySalary    string  `json:"prettySalary"`
	JobLevelVI      string  `json:"jobLevelVI"`
	YearsOfExp      int     `json:"yearsOfExperience"`
	NumberOfRecruit int     `json:"numberOfRecruits"`
	Skills          []struct {
		SkillName string `json:"skillName"`
	} `json:"skills"`
	Benefits []struct {
		BenefitName   string `json:"benefitName"`
		BenefitNameVI string `json:"benefitNameVI"`
		BenefitValue  string `json:"benefitValue"`
	} `json:"benefits"`
	WorkingLocations []struct {
		Address    string `json:"address"`
		CityNameVI string `json:"cityNameVI"`
	} `json:"workingLocations"`
	IndustriesV3 []struct {
		NameVI string `json:"industryV3NameVI"`
	} `json:"industriesV3"`
	JobFunctionsV3 struct {
		NameVI string `json:"jobFunctionV3NameVI"`
	} `json:"jobFunctionsV3"`
}

// ParseSearchResponse parses the JSON returned by URL_JOB_SEARCH.
func ParseSearchResponse(body []byte) (*SearchResult, error) {
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Meta.Code != 0 && resp.Meta.Code != 200 {
		return nil, fmt.Errorf("api error %d: %s", resp.Meta.Code, resp.Meta.Message)
	}

	now := time.Now().UTC()
	result := &SearchResult{
		Page:       resp.Meta.Page + 1,
		TotalPages: resp.Meta.NbPages,
		TotalJobs:  resp.Meta.NbHits,
		Jobs:       []Job{},
	}
	seen := make(map[string]struct{})
	for _, a := range resp.Data {
		if a.JobID == 0 {
			continue
		}
		job := a.toJob(now)
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		result.Jobs = append(result.Jobs, job)
	}
	return result, nil
}

func (a *apiJob) toJob(now time.Time) Job {
	j := Job{
		ID:              strconv.FormatInt(a.JobID, 10),
		Source:          Source,
		Title:           htmltext.Clean(a.JobTitle),
		URL:             a.JobURL,
		Company:         htmltext.Clean(a.CompanyName),
		CompanyID:       a.CompanyID,
		CompanyLogo:     a.CompanyLogo,
		Salary:          htmltext.Clean(a.PrettySalary),
		SalaryCurrency:  a.SalaryCurrency,
		Level:           a.JobLevelVI,
		JobFunction:     a.JobFunctionsV3.NameVI,
		ExperienceYears: a.YearsOfExp,
		Openings:        a.NumberOfRecruit,
		PostedAt:        a.ApprovedOn,
		ExpiredAt:       a.ExpiredOn,
		ScrapedAt:       now,
	}
	if j.URL == "" && a.Alias != "" {
		j.URL = fmt.Sprintf("%s/%s-%d-jv", siteURL, a.Alias, a.JobID)
	}
	if a.IsSalaryVisible {
		j.SalaryMin = positive(a.SalaryMin)
		j.SalaryMax = positive(a.SalaryMax)
	}

	var cities []string
	for _, l := range a.WorkingLocations {
		city := htmltext.Clean(l.CityNameVI)
		cities = appendUnique(cities, city)
		switch addr := htmltext.Clean(l.Address); {
		case addr != "" && city != "":
			j.Addresses = append(j.Addresses, city+": "+addr)
		case addr != "":
			j.Addresses = append(j.Addresses, addr)
		}
	}
	j.Location = strings.Join(cities, ", ")

	for _, s := range a.Skills {
		j.Skills = appendUnique(j.Skills, htmltext.Clean(s.SkillName))
	}
	for _, b := range a.Benefits {
		name := htmltext.Clean(b.BenefitNameVI)
		if name == "" {
			name = htmltext.Clean(b.BenefitName)
		}
		if v := htmltext.FromHTML(b.BenefitValue); v != "" {
			j.Benefits = append(j.Benefits, Benefit{Name: name, Value: v})
		}
	}
	for _, ind := range a.IndustriesV3 {
		j.Industries = appendUnique(j.Industries, htmltext.Clean(ind.NameVI))
	}
	return j
}

func positive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
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
