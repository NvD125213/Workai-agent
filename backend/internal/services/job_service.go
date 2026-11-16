package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pgvector/pgvector-go"
	"gorm.io/datatypes"

	"notification-job-mb/internal/models"
)

// ErrInvalidJobFile marks a data file that can never be imported as is
// (unreadable JSON, wrong shape). Retrying it is pointless.
var ErrInvalidJobFile = errors.New("job import: invalid data file")

// Column sizes from models.Job / models.JobSkill; longer values are cut so a
// single oversized field cannot make a whole file fail on every retry.
const (
	maxTitleLen     = 255
	maxCompanyLen   = 150
	maxSalaryLen    = 100
	maxSkillLen     = 100
	maxSourceLen    = 100
	maxSourceURLLen = 255
	maxUniqueIDLen  = 255

	maxLocationLen       = 100
	maxEmbeddingModelLen = 100
)

// JobFile is the document the AI service writes into the data folder after it
// has analyzed the jobs scraped by the adapters. A bare JSON array of
// AnalyzedJob is accepted too.
type JobFile struct {
	// Source and SourceURL are defaults for jobs that leave them empty.
	Source     string        `json:"source,omitempty"`
	SourceURL  string        `json:"source_url,omitempty"`
	AnalyzedAt *time.Time    `json:"analyzed_at,omitempty"`
	Jobs       []AnalyzedJob `json:"jobs"`
}

// AnalyzedJob is one job of a JobFile. ID, Source, URL and Title are required.
type AnalyzedJob struct {
	ID           string   `json:"id"`     // job ID on the source site
	Source       string   `json:"source"` // e.g. "topcv", "itviec"
	SourceURL    string   `json:"source_url,omitempty"`
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Company      string   `json:"company,omitempty"`
	Description  string   `json:"description,omitempty"`
	Requirements string   `json:"requirements,omitempty"`
	Benefits     string   `json:"benefits,omitempty"`
	Salary       string   `json:"salary,omitempty"`    // as displayed
	PostedAt     string   `json:"posted_at,omitempty"` // RFC 3339, yyyy-mm-dd or dd/mm/yyyy
	Deadline     string   `json:"deadline,omitempty"`  // last day to apply, same formats; empty if not shown
	Skills       []string `json:"skills,omitempty"`

	// Location and Experience are input for the AI analysis. Experience is never
	// stored; Location is stored only with the analysis below.
	Location   string `json:"location,omitempty"`
	Experience string `json:"experience,omitempty"`

	// Result of the AI analysis. A job with an Embedding is analyzed: these
	// fields are stored and Skills holds the AI's normalized skill names.
	MinYears       *int      `json:"min_years,omitempty"`
	RequiredYears  *int      `json:"required_years,omitempty"`
	IsRemote       bool      `json:"is_remote,omitempty"`
	Languages      []string  `json:"languages,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
}

// ImportResult counts the outcome of one data file.
type ImportResult struct {
	Imported int
	Skipped  int      // jobs rejected by validation
	Problems []string // why each skipped job was rejected
}

type jobStore interface {
	EnsureSource(ctx context.Context, name, url string) (uint, error)
	UpsertWithSkills(ctx context.Context, job *models.Job, skills []string) error
}

type JobService struct {
	jobs jobStore
}

func NewJobService(jobs jobStore) *JobService {
	return &JobService{jobs: jobs}
}

// ImportFile reads a data file and saves its jobs into Job and JobSkill.
//
// Jobs are upserted by "<source>:<id>", so importing the same file again is
// safe. An error wrapping ErrInvalidJobFile means the file is malformed; any
// other error is a storage failure and the file may be retried later.
func (s *JobService) ImportFile(ctx context.Context, path string) (ImportResult, error) {
	file, err := ReadJobFile(path)
	if err != nil {
		return ImportResult{}, err
	}
	return s.Import(ctx, file)
}

// ReadJobFile reads and decodes a JobFile. A decoding error wraps ErrInvalidJobFile.
func ReadJobFile(path string) (JobFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return JobFile{}, fmt.Errorf("job file: read %s: %w", path, err)
	}
	file, err := decodeJobFile(b)
	if err != nil {
		return JobFile{}, fmt.Errorf("%w: %s: %v", ErrInvalidJobFile, path, err)
	}
	return file, nil
}

// Import saves the jobs of file. Each job is written in its own transaction;
// on error, the jobs before it stay saved.
func (s *JobService) Import(ctx context.Context, file JobFile) (ImportResult, error) {
	var res ImportResult
	sourceIDs := make(map[string]uint)

	for i, in := range file.Jobs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if in.Source == "" {
			in.Source = file.Source
		}
		if in.SourceURL == "" {
			in.SourceURL = file.SourceURL
		}

		job, skills, err := toJobModel(in)
		if err != nil {
			res.Skipped++
			res.Problems = append(res.Problems, fmt.Sprintf("jobs[%d] (id=%q): %v", i, in.ID, err))
			continue
		}

		source := strings.ToLower(strings.TrimSpace(in.Source))
		id, ok := sourceIDs[source]
		if !ok {
			id, err = s.jobs.EnsureSource(ctx, truncate(source, maxSourceLen), sourceHome(in.SourceURL, job.URL))
			if err != nil {
				return res, fmt.Errorf("job import: %w", err)
			}
			sourceIDs[source] = id
		}
		job.SourceID = &id

		if err := s.jobs.UpsertWithSkills(ctx, job, skills); err != nil {
			return res, fmt.Errorf("job import: %w", err)
		}
		res.Imported++
	}
	return res, nil
}

func decodeJobFile(b []byte) (JobFile, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // BOM written by some Windows tools
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return JobFile{}, errors.New("empty file")
	}

	var file JobFile
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &file.Jobs); err != nil {
			return JobFile{}, err
		}
		return file, nil
	}
	if err := json.Unmarshal(trimmed, &file); err != nil {
		return JobFile{}, err
	}
	return file, nil
}

// toJobModel validates in and maps it to a Job (status AI_PROCESSED, since
// the AI service has already analyzed it) and a normalized skill list.
func toJobModel(in AnalyzedJob) (*models.Job, []string, error) {
	id := strings.TrimSpace(in.ID)
	source := strings.ToLower(strings.TrimSpace(in.Source))
	title := strings.TrimSpace(in.Title)
	jobURL := strings.TrimSpace(in.URL)

	switch {
	case id == "":
		return nil, nil, errors.New("missing id")
	case source == "":
		return nil, nil, errors.New("missing source")
	case title == "":
		return nil, nil, errors.New("missing title")
	case jobURL == "":
		return nil, nil, errors.New("missing url")
	}

	uniqueID := source + ":" + id
	if len(uniqueID) > maxUniqueIDLen {
		return nil, nil, fmt.Errorf("unique identifier longer than %d bytes", maxUniqueIDLen)
	}

	job := &models.Job{
		UniqueIdentifier: uniqueID,
		Title:            truncate(title, maxTitleLen),
		Company:          truncate(strings.TrimSpace(in.Company), maxCompanyLen),
		Description:      joinSections(in.Description, in.Requirements, in.Benefits),
		URL:              jobURL,
		RawSalary:        truncate(strings.TrimSpace(in.Salary), maxSalaryLen),
		Status:           models.JobStatusAIProcessed,
		PostedAt:         parsePostedAt(in.PostedAt),
	}
	if len(in.Embedding) > 0 {
		languages, err := json.Marshal(in.Languages)
		if err != nil {
			return nil, nil, fmt.Errorf("encode languages: %w", err)
		}
		vec := pgvector.NewVector(in.Embedding)
		now := time.Now()
		job.Location = truncate(strings.TrimSpace(in.Location), maxLocationLen)
		job.IsRemote = in.IsRemote
		job.MinYears = in.MinYears
		job.RequiredYears = in.RequiredYears
		job.Languages = datatypes.JSON(languages)
		job.Embedding = &vec
		job.EmbeddingModel = truncate(in.EmbeddingModel, maxEmbeddingModelLen)
		job.AnalyzedAt = &now
	}
	return job, normalizeSkills(in.Skills), nil
}

// joinSections stores description, requirements and benefits in the single
// Description column, each under a heading so they can be told apart.
func joinSections(description, requirements, benefits string) string {
	sections := []struct{ title, body string }{
		{"Mô tả công việc", description},
		{"Yêu cầu ứng viên", requirements},
		{"Quyền lợi", benefits},
	}
	var parts []string
	for _, s := range sections {
		if body := strings.TrimSpace(s.body); body != "" {
			parts = append(parts, "## "+s.title+"\n"+body)
		}
	}
	return strings.Join(parts, "\n\n")
}

// normalizeSkills trims, collapses inner whitespace and drops empty and
// case-insensitive duplicate names, keeping the first spelling seen.
func normalizeSkills(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = truncate(strings.Join(strings.Fields(s), " "), maxSkillLen)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

var postedAtLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "02/01/2006"}

// parsePostedAt returns nil for an empty or unrecognized date: the date is
// informational and must not cause the job to be rejected.
func parsePostedAt(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range postedAtLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// sourceHome returns sourceURL, or the scheme and host of jobURL when it is empty.
func sourceHome(sourceURL, jobURL string) string {
	if sourceURL = strings.TrimSpace(sourceURL); sourceURL != "" {
		return truncate(sourceURL, maxSourceURLLen)
	}
	if u, err := url.Parse(jobURL); err == nil && u.Host != "" {
		return truncate(u.Scheme+"://"+u.Host, maxSourceURLLen)
	}
	return ""
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
