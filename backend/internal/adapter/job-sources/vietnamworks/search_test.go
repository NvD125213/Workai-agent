package vietnamworks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// newTestClient talks to srv without any politeness delays.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.Client(), srv.URL)
	c.SetRequestDelay(0, 0)
	c.retryBase = 0
	c.batchPause = 0
	return c
}

// sampleSearchResponse is trimmed from a real URL_JOB_SEARCH response for "frontend developer".
const sampleSearchResponse = `{"meta":{"code":200,"message":"Success","nbHits":11,"page":0,"nbPages":1,"hitsPerPage":50},"data":[
{"jobId":2114360,"jobTitle":"Senior Fullstack Java Developer (Good At English) ",
 "jobUrl":"https://www.vietnamworks.com/senior-fullstack-java-developer-good-at-english-1790770451425939051-2114360-jv",
 "companyLogo":"https://images.vietnamworks.com/pictureofcompany/a6/11205218.png","companyName":"Công Ty TNHH LG CNS VIỆT NAM","companyId":375456,
 "approvedOn":"2026-09-30T19:14:11+07:00","expiredOn":"2026-10-30T23:59:59+07:00",
 "isSalaryVisible":false,"salary":0,"salaryMax":0,"salaryMin":0,"prettySalary":"Thương lượng","salaryCurrency":"USD","jobLevelVI":"Nhân viên",
 "skills":[{"skillId":33468,"skillName":"English Communication"},{"skillId":55363,"skillName":"Java Swing"},{"skillId":707132,"skillName":"Front-End"},{"skillId":575101,"skillName":"RESTful API"},{"skillId":1070398,"skillName":"RDBMS"}],
 "benefits":[{"benefitId":1,"benefitName":"Bonus","benefitNameVI":"Thưởng","benefitValue":"Health care: Premium health insurance, Annual health check-up"}],
 "workingLocations":[{"address":"Keangnam Landmark 72, Phạm Hùng, Phường Yên Hòa","cityId":24,"cityNameVI":"Hà Nội"}],
 "industriesV3":[{"industryV3Id":26,"industryV3NameVI":"Hệ thống CNTT & Thiết bị"}],
 "jobFunctionsV3":{"jobFunctionV3NameVI":"Phần Mềm Máy Tính"},"numberOfRecruits":1},
{"jobId":2117983,"jobTitle":"Lập Trình Viên Frontend (FE)","jobUrl":"https://www.vietnamworks.com/lap-trinh-vien-frontend-fe-1791543098739097616-2117983-jv",
 "companyName":"TECAPRO","isSalaryVisible":true,"salaryMin":18000000,"salaryMax":30000000,"prettySalary":"18tr-30tr ₫/tháng","salaryCurrency":"VND"},
{"jobId":2114360,"jobTitle":"duplicate"}
]}`

func TestParseSearchResponse(t *testing.T) {
	result, err := ParseSearchResponse([]byte(sampleSearchResponse))
	if err != nil {
		t.Fatalf("ParseSearchResponse: %v", err)
	}
	if result.Page != 1 || result.TotalPages != 1 || result.TotalJobs != 11 || len(result.Jobs) != 2 {
		t.Fatalf("got page %d/%d, total %d, %d jobs; want 1/1, 11, 2 (duplicate removed)", result.Page, result.TotalPages, result.TotalJobs, len(result.Jobs))
	}

	j := result.Jobs[0]
	checks := []struct{ name, got, want string }{
		{"id", j.ID, "2114360"},
		{"title", j.Title, "Senior Fullstack Java Developer (Good At English)"},
		{"url", j.URL, "https://www.vietnamworks.com/senior-fullstack-java-developer-good-at-english-1790770451425939051-2114360-jv"},
		{"company", j.Company, "Công Ty TNHH LG CNS VIỆT NAM"},
		{"salary", j.Salary, "Thương lượng"},
		{"location", j.Location, "Hà Nội"},
		{"level", j.Level, "Nhân viên"},
		{"posted_at", j.PostedAt, "2026-09-30T19:14:11+07:00"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if j.SalaryMin != nil || j.SalaryMax != nil {
		t.Errorf("hidden salary should have no range, got %v-%v", j.SalaryMin, j.SalaryMax)
	}
	if len(j.Skills) != 5 || j.Skills[0] != "English Communication" {
		t.Errorf("skills = %v", j.Skills)
	}
	if len(j.Benefits) == 0 || j.Benefits[0].Name != "Thưởng" {
		t.Errorf("benefits = %+v", j.Benefits)
	}
	if len(j.Addresses) == 0 || !strings.HasPrefix(j.Addresses[0], "Hà Nội: Keangnam Landmark 72") {
		t.Errorf("addresses = %v", j.Addresses)
	}

	// A job with a visible salary keeps its range.
	paid := result.Jobs[1]
	if paid.SalaryMin == nil || *paid.SalaryMin != 18000000 || paid.SalaryMax == nil || *paid.SalaryMax != 30000000 {
		t.Errorf("salary range = %v-%v, want 18000000-30000000", paid.SalaryMin, paid.SalaryMax)
	}
}

func TestSearchPayload(t *testing.T) {
	b, err := json.Marshal(newSearchPayload(" frontend developer ", 2, 50))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["query"] != "frontend developer" || got["page"] != 2.0 || got["hitsPerPage"] != 50.0 || got["userId"] != 0.0 {
		t.Errorf("payload = %s", b)
	}
	if !strings.Contains(string(b), `"value":"[{\"cityId\":24,\"districtId\":[-1]}]"`) {
		t.Errorf("payload is missing the Ha Noi district filter: %s", b)
	}
}

func searchPage(page, nbPages int, ids ...int) string {
	var data []string
	for _, id := range ids {
		data = append(data, fmt.Sprintf(`{"jobId":%d,"jobTitle":"Job %[1]d","jobUrl":"https://www.vietnamworks.com/job-%[1]d-jv","prettySalary":"Thương lượng"}`, id))
	}
	return fmt.Sprintf(`{"meta":{"code":200,"message":"Success","nbHits":%d,"page":%d,"nbPages":%d},"data":[%s]}`,
		len(ids), page, nbPages, strings.Join(data, ","))
}

func TestFetchAllJobs(t *testing.T) {
	pages := map[int]string{
		0: searchPage(0, 2, 1, 2),
		1: searchPage(1, 2, 2, 3),
	}
	var requested []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/job-search/v1.0/search" {
			http.NotFound(w, r)
			return
		}
		var p PayloadURLJobSearch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Query != "golang" {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		requested = append(requested, p.Page)
		fmt.Fprint(w, pages[p.Page])
	}))
	defer srv.Close()

	jobs, err := newTestClient(srv).FetchAllJobs(context.Background(), "golang", 5)
	if err != nil {
		t.Fatalf("FetchAllJobs: %v", err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	if !reflect.DeepEqual(ids, []string{"1", "2", "3"}) {
		t.Errorf("ids = %v, want [1 2 3]", ids)
	}
	if !reflect.DeepEqual(requested, []int{0, 1}) {
		t.Errorf("requested pages = %v, want [0 1]", requested)
	}
}

func TestFetchJobsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).FetchJobs(context.Background(), "golang", 1); err == nil {
		t.Fatal("expected error on 400")
	}
}
