package topcv

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

// newTestClient talks to srv without any politeness delays.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.Client(), srv.URL, "")
	c.SetRequestDelay(0, 0)
	c.retryBase = 0
	c.batchPause = 0
	return c
}

const sampleLD = `<script type="application/ld+json">{"@context":"http:\/\/schema.org\/","@type":"JobPosting",
"title":"MIDDLE FRONTEND DEVELOPER – Canvas LMS",
"description":"<h2>Mô tả công việc<\/h2><ul><li>Build UI<\/li><li> - • Write tests<\/li><\/ul><h2>Yêu cầu ứng viên<\/h2><p>2 năm React<\/p><h2>Quyền lợi<\/h2><ul><li>Thưởng Tết<\/li><\/ul>",
"datePosted":"2026-10-06","validThrough":"2026-11-05T23:59:59+07:00","employmentType":"FULL_TIME",
"hiringOrganization":{"@type":"Organization","name":"Edufit","sameAs":"https:\/\/www.topcv.vn\/cong-ty\/edufit\/18476.html"},
"baseSalary":{"@type":"MonetaryAmount","currency":"VND","value":{"@type":"QuantitativeValue","unitText":"MONTH","maxValue":25000000}},
"industry":"IT phần mềm","occupationalCategory":"Nhân viên",
"experienceRequirements":{"@type":"OccupationalExperienceRequirements","monthsOfExperience":24},
"skills":"CSS, Git, React","totalJobOpenings":2}</script>`

const sampleDetailBody = `
<div class="box-header-job">
  <h1 class="box-header-job__title">
    MIDDLE <a href="#" class="text-highlight">FRONTEND DEVELOPER</a> – Canvas LMS
    <span class="icon-verified-employer"><i title="Nhà tuyển dụng đã xác thực"></i>Đã xác thực</span>
  </h1>
  <span class="box-header-job__salary--title"> Tới 25 triệu </span>
  <div class="box-header-job-list-info">
    <div class="box-header-job-list-info__item"><div class="list-info__content__title">Địa điểm</div><div class="list-info__content__desc"><a>Hà Nội</a></div></div>
    <div class="box-header-job-list-info__item"><div class="list-info__content__title">Kinh nghiệm</div><div class="list-info__content__desc"> 2 năm </div></div>
    <div class="box-header-job-list-info__item"><div class="list-info__content__title">Hạn ứng tuyển</div><div class="list-info__content__desc">05/11/2026</div></div>
  </div>
</div>
<div class="box-job-information-detail-item">
  <div class="box-job-information-detail-item__title"><h2 class="box-job-information-detail-item__title--title"> Mô tả công việc </h2></div>
  <div class="box-job-information-detail-item__text">
    <p><strong>&nbsp; 1. Phát triển UI</strong></p><ul><li>Customize frontend Canvas.</li><li><p>Phối hợp Backend.</p><p><strong>2. Tích hợp</strong></p></li></ul>
  </div>
</div>
<div class="box-job-information-detail-item box-job-information-required-candidate">
  <div class="box-job-information-detail-item__title"><h2 class="box-job-information-detail-item__title--title">Yêu cầu ứng viên</h2></div>
  <div class="box-job-information-detail-item__text"><ul><li>Đại học CNTT</li><li>2–4 năm kinh nghiệm</li></ul></div>
  <div class="required-tags">
    <div class="required-tag"><h3 class="required-tag__content--title">Kiến thức ngành</h3><div class="required-tag__content--desc">IT - Phần mềm</div></div>
    <div class="required-tag"><h3 class="required-tag__content--title">Kỹ năng cần có</h3><div class="required-tag__content--desc">CSS, Git, React</div></div>
    <div class="required-tag"><h3 class="required-tag__content--title">Kỹ năng nên có</h3><div class="required-tag__content--desc">Redis, Docker</div></div>
  </div>
</div>
<div class="box-job-information-detail-item box-job-information-benefit">
  <div class="box-job-information-detail-item__title"><h2 class="box-job-information-detail-item__title--title">Quyền lợi ứng viên</h2></div>
  <div class="box-job-information-detail-item__text"><ul>
<li>Thu nhập &amp; bảo hiểm</li>
<li>Thưởng Tết</li>
</ul></div>
  <div class="required-tags"><div class="required-tags__title">Quyền lợi bổ sung</div><div class="required-tags__list">
    <div class="required-tag"><h3 class="required-tag__content--title">Phụ cấp</h3><div class="required-tag__content--desc">Ăn trưa</div></div>
  </div></div>
</div>
<div class="box-job-information-detail-item box-job-information-address-and-time">
  <div class="box-job-information-address-and-time-list"><div class="box-job-information-address-and-time-list__item">
    <h3 class="box-job-information-address-and-time-list__item--title">Địa điểm làm việc</h3>
    <div class="box-job-information-address-and-time-list__item--content"><ul><li><strong>Hà Nội:</strong> Tây Hồ Tây Starlake</li></ul></div>
  </div></div>
  <div class="box-job-information-address-and-time-list"><div class="box-job-information-address-and-time-list__item">
    <h3 class="box-job-information-address-and-time-list__item--title">Thời gian làm việc</h3>
    <div class="box-job-information-address-and-time-list__item--content"><ul><li>Thứ 2 - Thứ 6 (từ 08:00 đến 17:00)</li></ul></div>
  </div></div>
  <div class="box-job-information-address-and-time-list"><div class="box-job-information-address-and-time-list__item">
    <h3 class="box-job-information-address-and-time-list__item--title">Cách thức ứng tuyển</h3>
    <div class="box-job-information-address-and-time-list__item--content"><ul><li>Bấm <b>Ứng tuyển</b> ngay.</li></ul></div>
  </div></div>
</div>
<div class="box-job-information-general-info-list__item"><div class="box-job-information-general-info-list__item--content-title">Cấp bậc</div><div class="box-job-information-general-info-list__item--content-desc">Nhân viên</div></div>
<div class="box-job-information-general-info-list__item"><div class="box-job-information-general-info-list__item--content-title">Học vấn</div><div class="box-job-information-general-info-list__item--content-desc">Cao Đẳng trở lên</div></div>
<div class="box-job-information-general-info-list__item"><div class="box-job-information-general-info-list__item--content-title">Số lượng tuyển</div><div class="box-job-information-general-info-list__item--content-desc">2 người</div></div>
<div class="box-job-information-general-info-list__item"><div class="box-job-information-general-info-list__item--content-title">Hình thức làm việc</div><div class="box-job-information-general-info-list__item--content-desc">Làm việc tại văn phòng / Onsite</div></div>
`

func detailPage(body string) string {
	return "<html><head>" + sampleLD + "</head><body>" + body + "</body></html>"
}

func TestParseJobDetail(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(detailPage(sampleDetailBody)))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}

	maxSalary, months := 25000000.0, 24
	wantOverview := Overview{
		Salary:           "Tới 25 triệu",
		SalaryMax:        &maxSalary,
		SalaryCurrency:   "VND",
		SalaryUnit:       "MONTH",
		Location:         "Hà Nội",
		Experience:       "2 năm",
		ExperienceMonths: &months,
		Deadline:         "05/11/2026",
		ValidThrough:     "2026-11-05T23:59:59+07:00",
		DatePosted:       "2026-10-06",
		Level:            "Nhân viên",
		Education:        "Cao Đẳng trở lên",
		Openings:         "2 người",
		WorkType:         "Làm việc tại văn phòng / Onsite",
		EmploymentType:   "FULL_TIME",
		Industry:         "IT phần mềm",
	}
	if !reflect.DeepEqual(d.Overview, wantOverview) {
		t.Errorf("overview\n got: %+v\nwant: %+v", d.Overview, wantOverview)
	}

	checks := []struct{ name, got, want string }{
		{"title", d.Title, "MIDDLE FRONTEND DEVELOPER – Canvas LMS"},
		{"company", d.Company, "Edufit"},
		{"company_url", d.CompanyURL, "https://www.topcv.vn/cong-ty/edufit/18476.html"},
		{"description", d.Description, "1. Phát triển UI\n- Customize frontend Canvas.\n- Phối hợp Backend.\n2. Tích hợp"},
		{"requirements", d.Requirements, "- Đại học CNTT\n- 2–4 năm kinh nghiệm"},
		{"benefits", d.Benefits, "- Thu nhập & bảo hiểm\n- Thưởng Tết"},
		{"how_to_apply", d.HowToApply, "Bấm Ứng tuyển ngay."},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	wantSkills := Skills{Required: []string{"CSS", "Git", "React"}, Preferred: []string{"Redis", "Docker"}, All: []string{"CSS", "Git", "React"}}
	if !reflect.DeepEqual(d.Skills, wantSkills) {
		t.Errorf("skills = %+v, want %+v", d.Skills, wantSkills)
	}
	if !reflect.DeepEqual(d.RequirementTags, map[string]string{"Kiến thức ngành": "IT - Phần mềm"}) {
		t.Errorf("requirement_tags = %v", d.RequirementTags)
	}
	if !reflect.DeepEqual(d.BenefitTags, map[string]string{"Phụ cấp": "Ăn trưa"}) {
		t.Errorf("benefit_tags = %v", d.BenefitTags)
	}
	if !reflect.DeepEqual(d.WorkLocations, []string{"Hà Nội: Tây Hồ Tây Starlake"}) {
		t.Errorf("work_locations = %v", d.WorkLocations)
	}
	if !reflect.DeepEqual(d.WorkingTimes, []string{"Thứ 2 - Thứ 6 (từ 08:00 đến 17:00)"}) {
		t.Errorf("working_times = %v", d.WorkingTimes)
	}
}

// An unknown layout (e.g. brand pages) still yields sections from the JSON-LD description.
func TestParseJobDetailFallsBackToJSONLD(t *testing.T) {
	d, err := ParseJobDetail(strings.NewReader(detailPage(`<div class="brand-layout">x</div>`)))
	if err != nil {
		t.Fatalf("ParseJobDetail: %v", err)
	}
	if d.Description != "- Build UI\n- Write tests" || d.Requirements != "2 năm React" || d.Benefits != "- Thưởng Tết" {
		t.Errorf("got description=%q requirements=%q benefits=%q", d.Description, d.Requirements, d.Benefits)
	}
	if d.Title != "MIDDLE FRONTEND DEVELOPER – Canvas LMS" || d.Company != "Edufit" {
		t.Errorf("got title=%q company=%q", d.Title, d.Company)
	}
}

func TestParseJobDetailEmptyPage(t *testing.T) {
	if _, err := ParseJobDetail(strings.NewReader("<html><body>nothing</body></html>")); err == nil {
		t.Fatal("expected error for a page without job content")
	}
}

func TestFetchJobDetails(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/viec-lam/a/1.html":
			fmt.Fprint(w, detailPage(sampleDetailBody))
		case "/viec-lam/b/2.html":
			// Rate limited once, then fine: the client must retry.
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, detailPage(sampleDetailBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	jobs := []Job{
		{ID: "1", URL: "https://www.topcv.vn/viec-lam/a/1.html?ta_source=x"},
		{ID: "2", URL: "https://www.topcv.vn/viec-lam/b/2.html"},
		{ID: "3", URL: "https://www.topcv.vn/viec-lam/gone/3.html"},
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
	if details[0].URL != "https://www.topcv.vn/viec-lam/a/1.html" {
		t.Errorf("url = %q, want tracking params stripped", details[0].URL)
	}
	if len(failures) != 1 || failures[0].ID != "3" || !strings.Contains(failures[0].Error, "not found") {
		t.Errorf("failures = %+v, want job 3 not found", failures)
	}
}

func TestFetchJobDetailsStopsWhenBlocked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	jobs := []Job{
		{ID: "1", URL: "https://www.topcv.vn/viec-lam/a/1.html"},
		{ID: "2", URL: "https://www.topcv.vn/viec-lam/b/2.html"},
	}
	details, _, err := newTestClient(srv).FetchJobDetails(context.Background(), jobs, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(details) != 0 || hits.Load() != 1 {
		t.Errorf("got %d details after %d requests; want 0 details and a single request", len(details), hits.Load())
	}
}
