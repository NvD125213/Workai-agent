package vietnamworks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const sampleDetail = `{"status":"success","message":"Success","data":{"job_id":"%s",
"job_title":"Senior Fullstack Java Developer (Good At English) ","job_level_name":"Experienced (non-manager)",
"job_function_name":"Software Developer","job_location_city":"Ha Noi","company_id":375456,
"company_name":"Công Ty TNHH LG CNS VIỆT NAM","company_industry":[{"id":"26","name":"IT System & Devices"}],
"is_salary_hidden":false,"salary_range":"18000000-30000000",
"job_requirement":"<p>- 5 years of Java</p><p>- English&nbsp;(Toeic 700+)</p>",
"job_description":"<p>Build insurance platform.</p><ul><li> - Design channels</li><li>Integrate APIs</li></ul>",
"number_of_years_of_experience_required":5,"headcount":2,"job_expired_date":"2026-10-30"}}`

const notFoundDetail = `{"status":"success","message":"Success","data":{"job_id":"1","job_location_city":null,"company_industry":null,"is_salary_hidden":true,"salary_range":"0-0"}}`

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail([]byte(fmt.Sprintf(sampleDetail, "2114360")))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}
	minS, maxS, months := 18000000.0, 30000000.0, 60
	wantOverview := Overview{
		SalaryMin:        &minS,
		SalaryMax:        &maxS,
		Location:         "Ha Noi",
		Experience:       "5 năm",
		ExperienceMonths: &months,
		Deadline:         "2026-10-30",
		Level:            "Experienced (non-manager)",
		Openings:         "2",
		Industry:         "IT System & Devices",
		JobFunction:      "Software Developer",
	}
	if !reflect.DeepEqual(d.Overview, wantOverview) {
		t.Errorf("overview\n got: %+v\nwant: %+v", d.Overview, wantOverview)
	}
	checks := []struct{ name, got, want string }{
		{"id", d.ID, "2114360"},
		{"title", d.Title, "Senior Fullstack Java Developer (Good At English)"},
		{"company", d.Company, "Công Ty TNHH LG CNS VIỆT NAM"},
		{"description", d.Description, "Build insurance platform.\n- Design channels\n- Integrate APIs"},
		{"requirements", d.Requirements, "- 5 years of Java\n- English (Toeic 700+)"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestParseJobDetailNotFound(t *testing.T) {
	if _, err := ParseJobDetail([]byte(notFoundDetail)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchJobDetails(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api-gateway/v1.0/tracking/job-info" {
			http.NotFound(w, r)
			return
		}
		switch id := r.URL.Query().Get("jobId"); id {
		case "1":
			fmt.Fprintf(w, sampleDetail, id)
		case "2":
			// Rate limited once, then fine: the client must retry.
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprintf(w, sampleDetail, id)
		default:
			fmt.Fprint(w, notFoundDetail)
		}
	}))
	defer srv.Close()

	maxS := 25000000.0
	jobs := []Job{
		{
			ID: "1", URL: "https://www.vietnamworks.com/a-1-jv", Title: "Job One", Salary: "Tới 25tr ₫/tháng",
			SalaryMax: &maxS, SalaryCurrency: "VND", Location: "Hà Nội", Level: "Nhân viên",
			Skills:    []string{"Java", "RESTful API"},
			Benefits:  []Benefit{{Name: "Thưởng", Value: "Thưởng Tết"}, {Name: "Thưởng", Value: "Lương tháng 13"}},
			Addresses: []string{"Hà Nội: Keangnam"},
			PostedAt:  "2026-09-30T19:14:11+07:00",
		},
		{ID: "2", URL: "https://www.vietnamworks.com/b-2-jv"},
		{ID: "3", URL: "https://www.vietnamworks.com/gone-3-jv"},
	}
	var seen []string
	details, failures, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, func(d JobDetail) {
		seen = append(seen, d.ID)
	})
	if err != nil {
		t.Fatalf("FetchJobDetails: %v", err)
	}
	if len(details) != 2 || !reflect.DeepEqual(seen, []string{"1", "2"}) {
		t.Fatalf("got %d details, callback ids %v; want 2 and [1 2]", len(details), seen)
	}

	d := details[0]
	if d.URL != jobs[0].URL || d.Title != "Job One" || d.Overview.Salary != "Tới 25tr ₫/tháng" ||
		d.Overview.Location != "Hà Nội" || d.Overview.Level != "Nhân viên" || d.Overview.DatePosted != jobs[0].PostedAt {
		t.Errorf("search data not merged: %+v", d)
	}
	if d.Overview.SalaryMin != nil || d.Overview.SalaryMax == nil || *d.Overview.SalaryMax != maxS {
		t.Errorf("salary = %v-%v, want the search range", d.Overview.SalaryMin, d.Overview.SalaryMax)
	}
	if !reflect.DeepEqual(d.Skills.All, []string{"Java", "RESTful API"}) || !reflect.DeepEqual(d.WorkLocations, []string{"Hà Nội: Keangnam"}) {
		t.Errorf("skills = %v, work_locations = %v", d.Skills.All, d.WorkLocations)
	}
	if d.Benefits != "- Thưởng Tết\n- Lương tháng 13" || d.BenefitTags["Thưởng"] != "Thưởng Tết; Lương tháng 13" {
		t.Errorf("benefits = %q, tags = %v", d.Benefits, d.BenefitTags)
	}
	// Without search data the job info API values are kept.
	if details[1].Overview.Location != "Ha Noi" || details[1].Overview.SalaryMin == nil {
		t.Errorf("job 2 overview = %+v", details[1].Overview)
	}

	if len(failures) != 1 || failures[0].ID != "3" || !strings.Contains(failures[0].Error, "not found") {
		t.Errorf("failures = %+v, want job 3 not found", failures)
	}
}

func TestFetchJobDetailsStopsWhenBlocked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	jobs := []Job{{ID: "1"}, {ID: "2"}}
	details, _, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 0 || hits.Load() != 1 {
		t.Errorf("got %d details after %d requests; want 0 details and a single request", len(details), hits.Load())
	}
}
