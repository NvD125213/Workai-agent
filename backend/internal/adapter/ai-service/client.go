// Package aiservice calls the internal AI service (llm/api.py): CV parsing,
// profile analysis with embedding, and job scoring.
package aiservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes bounds what is read from the AI service; a /score answer
// for a few hundred jobs is well under this.
const maxResponseBytes = 32 << 20

// Error is a non-2xx answer of the AI service. Status 4xx means the input was
// rejected (unsupported file, scanned CV...), 5xx/502 that the service failed.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ai service: %d %s", e.Status, e.Detail)
}

// IsInputError reports whether err is the AI service rejecting the input, so
// the message can be shown to the user as is.
func IsInputError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500
}

type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient: hc nil uses a client without overall timeout; every call is
// bounded by its context instead, since /score can take tens of minutes
// (FIND_JOB_SCORE_TIMEOUT_MINUTES). ResponseHeaderTimeout is only a backstop
// for a hung service and must stay above that setting.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 2 * time.Hour,
			IdleConnTimeout:       90 * time.Second,
		}}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), hc: hc}
}

// ParseCV extracts the text of a CV file (pdf, docx, txt, md).
func (c *Client) ParseCV(ctx context.Context, filename string, file io.Reader) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("ai service: build upload: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("ai service: read upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("ai service: build upload: %w", err)
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := c.do(ctx, "/parse-cv", mw.FormDataContentType(), &body, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// Analysis is the answer of /analyze-cv.
type Analysis struct {
	Profile        json.RawMessage `json:"profile"` // full profile, sent back as is to /score
	UserProfile    ProfileColumns  `json:"user_profile"`
	UserSkills     []SkillColumn   `json:"user_skills"`
	Embedding      []float32       `json:"embedding"`
	EmbeddingModel string          `json:"embedding_model"`
	DesiredTitle   string          `json:"desired_title"`  // current title from the CV
	SearchKeyword  string          `json:"search_keyword"` // broad keyword, e.g. "Developer"
}

// ProfileColumns are the user_profiles values computed by the AI service.
type ProfileColumns struct {
	ResumeHash   string          `json:"resume_hash"`
	ExpYears     int             `json:"exp_years"`
	CurrentTitle string          `json:"current_title"`
	Summary      string          `json:"summary"`
	Education    string          `json:"education"`
	Location     string          `json:"location"`
	Languages    json.RawMessage `json:"languages"`
}

type SkillColumn struct {
	SkillName string `json:"skill_name"`
	Level     string `json:"level"`
}

// AnalyzeCV turns CV text into a structured profile with its embedding.
func (c *Client) AnalyzeCV(ctx context.Context, cvText string) (*Analysis, error) {
	var out Analysis
	if err := c.postJSON(ctx, "/analyze-cv", map[string]string{"cv_text": cvText}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ScoreJob is one crawled job sent for scoring (a services.AnalyzedJob).
type ScoreJob struct {
	ID           string   `json:"id"`
	Source       string   `json:"source"`
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Company      string   `json:"company,omitempty"`
	Description  string   `json:"description,omitempty"`
	Requirements string   `json:"requirements,omitempty"`
	Skills       []string `json:"skills,omitempty"`
	Location     string   `json:"location,omitempty"`
	Experience   string   `json:"experience,omitempty"`
	Salary       string   `json:"salary,omitempty"`
	Deadline     string   `json:"deadline,omitempty"` // RFC 3339, yyyy-mm-dd or dd/mm/yyyy
}

type ScoreRequest struct {
	Profile         json.RawMessage `json:"profile"`
	CVText          string          `json:"cv_text"`
	DesiredTitle    string          `json:"desired_title,omitempty"`
	DesiredLocation string          `json:"desired_location,omitempty"`
	Jobs            []ScoreJob      `json:"jobs"`
	// SearchID makes the AI service write llm/data/reports/find_job_<SearchID>.md.
	SearchID uint   `json:"search_id,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	// ExpFilter drops jobs outside the experience range of the CV and ranks the
	// rest by salary, then by required experience.
	ExpFilter bool `json:"exp_filter,omitempty"`
}

// ScoreResult: JobID of every entry is "<source>:<id>", i.e. jobs.unique_identifier.
type ScoreResult struct {
	Results      []ScoredJob        `json:"results"`  // passed the hard filters, best first
	Rejected     []RejectedJob      `json:"rejected"` // removed by the hard filters
	Summary      ScoreSummary       `json:"summary"`
	Timings      map[string]float64 `json:"timings"`
	ModelVersion string             `json:"model_version"`
	Threshold    float64            `json:"threshold"`
	Report       string             `json:"report,omitempty"` // path of the markdown report
}

// ScoreSummary counts the jobs at each step of the scoring.
type ScoreSummary struct {
	Crawled       int           `json:"crawled"`
	Rejected      int           `json:"rejected"`
	Scored        int           `json:"scored"`
	Passed        int           `json:"passed"`
	RejectReasons []ReasonCount `json:"reject_reasons"`
}

type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type ScoredJob struct {
	JobID         string              `json:"job_id"`
	Rank          int                 `json:"rank"`
	Score         float64             `json:"score"`
	Passed        bool                `json:"passed"`
	Penalty       float64             `json:"penalty"`
	Components    map[string]*float64 `json:"components"`
	MatchedSkills []string            `json:"matched_skills"`
	MissingSkills []string            `json:"missing_skills"`
	JobData       *JobData            `json:"job_data,omitempty"` // only for Passed jobs
}

// JobData is the analysis computed while scoring, to store with the job.
type JobData struct {
	MinYears       *int      `json:"min_years"`
	RequiredYears  *int      `json:"required_years"`
	Location       string    `json:"location"`
	IsRemote       bool      `json:"is_remote"`
	Languages      []string  `json:"languages"`
	Skills         []string  `json:"skills"`
	Embedding      []float32 `json:"embedding"`
	EmbeddingModel string    `json:"embedding_model"`
}

type RejectedJob struct {
	JobID         string   `json:"job_id"`
	RejectReasons []string `json:"reject_reasons"`
	MatchedSkills []string `json:"matched_skills"`
	MissingSkills []string `json:"missing_skills"`
}

// Score applies the hard filters to jobs and scores the rest against the profile.
func (c *Client) Score(ctx context.Context, req ScoreRequest) (*ScoreResult, error) {
	var out ScoreResult
	if err := c.postJSON(ctx, "/score", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("ai service: encode %s: %w", path, err)
	}
	return c.do(ctx, path, "application/json", bytes.NewReader(b), out)
}

func (c *Client) do(ctx context.Context, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("ai service: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("ai service %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ai service %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Status: resp.StatusCode, Detail: errorDetail(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ai service %s: decode response: %w", path, err)
	}
	return nil
}

// errorDetail extracts FastAPI's {"detail": ...}; detail is a string for
// HTTPException and a list of objects for request validation errors.
func errorDetail(body []byte) string {
	var e struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Detail) > 0 {
		var s string
		if json.Unmarshal(e.Detail, &s) == nil {
			return s
		}
		return string(e.Detail)
	}
	const maxLen = 500
	if len(body) > maxLen {
		body = body[:maxLen]
	}
	return strings.TrimSpace(string(body))
}
