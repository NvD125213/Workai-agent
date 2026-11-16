package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/datatypes"

	aiservice "notification-job-mb/internal/adapter/ai-service"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

var (
	// ErrProfileNotAnalyzed: find-job needs an analyzed CV (POST /analyze-cv).
	ErrProfileNotAnalyzed = errors.New("find job: CV has not been analyzed yet")
	// ErrNoKeyword: neither the request nor the analyzed CV gives a search keyword.
	ErrNoKeyword = errors.New("find job: no search keyword")
	// ErrSearchInProgress: the user already has an unfinished search.
	ErrSearchInProgress = repositories.ErrSearchInProgress
	// ErrShuttingDown: the server is stopping and accepts no new search.
	ErrShuttingDown = errors.New("find job: server is shutting down")
	// ErrAnalysisNotDone: the CV analysis given to find-job is still running or failed.
	ErrAnalysisNotDone = errors.New("find job: CV analysis is not done")
	// ErrAnalysisOutdated: a newer CV was uploaded after the CV analysis given to find-job.
	ErrAnalysisOutdated = errors.New("find job: CV analysis is outdated")
)

const (
	finishTimeout = 10 * time.Second // final DB writes, also during shutdown
	maxErrorLen   = 1000
)

// ScoreTimeoutError starts the error saved on a search whose AI scoring ran past
// FindJobConfig.ScoreTimeout (too many crawled jobs for the scoring hardware).
const ScoreTimeoutError = "score timeout"

type FindJobConfig struct {
	MaxPages      int           // search pages per source; default 1
	MaxConcurrent int           // searches crawling/scoring at the same time; default 1
	ScoreTimeout  time.Duration // AI scoring of one search; default 30 min
}

func (c *FindJobConfig) applyDefaults() {
	if c.MaxPages <= 0 {
		c.MaxPages = 1
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 1
	}
	if c.ScoreTimeout <= 0 {
		c.ScoreTimeout = 30 * time.Minute
	}
}

type searchStore interface {
	Create(ctx context.Context, s *models.JobSearch) error
	SetStatus(ctx context.Context, id uint, status string) error
	Finish(ctx context.Context, id uint, status, errMsg string, jobsFound, matchesFound int, summary datatypes.JSON) error
	FindForUser(ctx context.Context, userID, id uint) (*models.JobSearch, error)
	SaveMatches(ctx context.Context, matches []models.AIMatch) error
	ListMatches(ctx context.Context, searchID uint) ([]models.AIMatch, error)
}

type profileFinder interface {
	FindByUserID(ctx context.Context, userID uint) (*models.UserProfile, error)
}

type analysisFinder interface {
	FindForUser(ctx context.Context, userID, id uint) (*models.CVAnalysis, error)
}

type jobImportFunc func(ctx context.Context, file services.JobFile) (services.ImportResult, error)

type jobIDFinder interface {
	IDsByUniqueIDs(ctx context.Context, uniqueIDs []string) (map[string]uint, error)
}

type jobScorer interface {
	Score(ctx context.Context, req aiservice.ScoreRequest) (*aiservice.ScoreResult, error)
}

type crawlFunc func(ctx context.Context, keyword string, maxPages int) []SourceResult

// FindJobs runs job searches for users in the background: crawl every job
// source for the keyword of the user's CV, save the jobs, score them with the
// AI service and save the matches. Progress is visible through the
// job_searches row: QUEUED -> CRAWLING -> RANKING -> DONE / FAILED.
type FindJobs struct {
	searches searchStore
	profiles profileFinder
	analyses analysisFinder
	importFn jobImportFunc
	jobIDs   jobIDFinder
	ai       jobScorer
	crawl    crawlFunc
	cfg      FindJobConfig

	ctx    context.Context // cancelled by Shutdown; parent of every running search
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{} // bounds searches running at once
}

func NewFindJobs(searches searchStore, profiles profileFinder, analyses analysisFinder, jobService *services.JobService,
	jobIDs jobIDFinder, ai jobScorer, cfg FindJobConfig) *FindJobs {
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &FindJobs{
		searches: searches, profiles: profiles, analyses: analyses, importFn: jobService.Import, jobIDs: jobIDs, ai: ai,
		crawl: CrawlAllSources, cfg: cfg,
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, cfg.MaxConcurrent),
	}
}

// StartRequest: empty fields fall back to what the CV analysis found.
type StartRequest struct {
	AnalysisID   uint   // the DONE CV analysis (POST /analyze-cv) to search for
	ExpFilter    bool   // keep only jobs in the experience range of the CV, rank by salary then experience
	DesiredTitle string // used for the title score; default: current title in the CV
	Keyword      string // crawl keyword; default: broad keyword from the CV, e.g. "Developer"
	Location     string
}

// Start queues a search for userID and returns it at once; the work runs in
// the background. repositories.ErrNotFound when the analysis does not exist or
// belongs to someone else.
func (f *FindJobs) Start(ctx context.Context, userID uint, req StartRequest) (*models.JobSearch, error) {
	if f.ctx.Err() != nil {
		return nil, ErrShuttingDown
	}
	a, err := f.analyses.FindForUser(ctx, userID, req.AnalysisID)
	if err != nil {
		return nil, err
	}
	if a.Status != models.AnalysisStatusDone {
		return nil, ErrAnalysisNotDone
	}
	p, err := f.profiles.FindByUserID(ctx, userID)
	if errors.Is(err, repositories.ErrNotFound) || (err == nil && (p.ParsedAt == nil || len(p.Analysis) == 0)) {
		return nil, ErrProfileNotAnalyzed
	}
	if err != nil {
		return nil, err
	}
	if p.ResumeHash != a.ResumeHash {
		return nil, ErrAnalysisOutdated
	}

	keyword := firstNonEmpty(req.Keyword, p.SearchKeyword)
	if keyword == "" {
		return nil, ErrNoKeyword
	}
	s := &models.JobSearch{
		UserID:       userID,
		AnalysisID:   &a.ID,
		ExpFilter:    req.ExpFilter,
		DesiredTitle: firstNonEmpty(req.DesiredTitle, p.CurrentTitle, keyword),
		Keyword:      keyword,
		Location:     strings.TrimSpace(req.Location),
		Status:       models.SearchStatusQueued,
	}
	if err := f.searches.Create(ctx, s); err != nil {
		return nil, err
	}
	f.wg.Go(func() { f.run(s, p) })
	return s, nil
}

// SearchView is a search with its matches, best first.
type SearchView struct {
	Search  *models.JobSearch
	Matches []models.AIMatch
}

// Get returns the search id of userID; repositories.ErrNotFound when it does
// not exist or belongs to someone else.
func (f *FindJobs) Get(ctx context.Context, userID, id uint) (*SearchView, error) {
	s, err := f.searches.FindForUser(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	view := &SearchView{Search: s}
	if s.Status == models.SearchStatusDone {
		if view.Matches, err = f.searches.ListMatches(ctx, s.ID); err != nil {
			return nil, err
		}
	}
	return view, nil
}

// Shutdown stops accepting searches, cancels the running ones (they end as
// FAILED) and waits for them until ctx ends.
func (f *FindJobs) Shutdown(ctx context.Context) error {
	f.cancel()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *FindJobs) run(s *models.JobSearch, p *models.UserProfile) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("find job %d: panic: %v", s.ID, r)
			f.finish(s.ID, models.SearchStatusFailed, fmt.Sprintf("internal error: %v", r), outcome{})
		}
	}()

	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-f.ctx.Done():
		f.finish(s.ID, models.SearchStatusFailed, "server stopped before the search started", outcome{})
		return
	}

	o, err := f.search(s, p)
	if err != nil {
		if f.ctx.Err() != nil {
			err = errors.New("server stopped during the search")
		}
		log.Printf("find job %d: %v", s.ID, err)
		o.matches = 0 // nothing is shown for a failed search
		f.finish(s.ID, models.SearchStatusFailed, err.Error(), o)
		return
	}
	log.Printf("find job %d: done, %d job(s) crawled, %d match(es)", s.ID, o.summary.Crawled, o.matches)
	f.finish(s.ID, models.SearchStatusDone, o.warn, o)
}

// outcome is what a search produced, also when it failed part way.
type outcome struct {
	matches int
	warn    string // sources that failed while the others still produced a result
	summary searchSummary
}

// searchSummary is stored in job_searches.summary. Only the matched jobs are
// saved in jobs, so this is the only record of the jobs that were dropped.
type searchSummary struct {
	Crawled       int                     `json:"crawled"`
	Rejected      int                     `json:"rejected"` // by the hard filters
	Scored        int                     `json:"scored"`   // passed the hard filters
	Passed        int                     `json:"passed"`   // reached the score threshold, saved
	Sources       []sourceSummary         `json:"sources"`
	RejectReasons []aiservice.ReasonCount `json:"reject_reasons,omitempty"`
	Report        string                  `json:"report,omitempty"` // markdown report written by the AI service
}

type sourceSummary struct {
	Source  string `json:"source"`
	Crawled int    `json:"crawled"`
	Passed  int    `json:"passed"`
	Error   string `json:"error,omitempty"`
}

// search does the work of one search: crawl, score, then save only the jobs
// that reached the score threshold, with the analysis computed while scoring.
func (f *FindJobs) search(s *models.JobSearch, p *models.UserProfile) (outcome, error) {
	var o outcome
	ctx := f.ctx
	if err := f.searches.SetStatus(ctx, s.ID, models.SearchStatusCrawling); err != nil {
		return o, err
	}

	// 1) Crawl every source for the keyword
	var crawled []services.AnalyzedJob
	var sourceErrs []string
	for _, r := range f.crawl(ctx, s.Keyword, f.cfg.MaxPages) {
		crawled = append(crawled, r.Jobs...)
		src := sourceSummary{Source: r.Source, Crawled: len(r.Jobs)}
		if r.Err != nil {
			src.Error = truncateRunes(r.Err.Error(), 300)
			sourceErrs = append(sourceErrs, fmt.Sprintf("%s: %v", r.Source, r.Err))
		}
		o.summary.Sources = append(o.summary.Sources, src)
	}
	o.summary.Crawled = len(crawled)
	o.warn = strings.Join(sourceErrs, "; ")
	if ctx.Err() != nil {
		return o, ctx.Err()
	}
	if len(crawled) == 0 {
		if len(sourceErrs) > 0 {
			return o, fmt.Errorf("every source failed: %s", o.warn)
		}
		return o, nil // nothing posted for this keyword: DONE with 0 results
	}

	// 2) Score in memory: hard filters, then the soft score (rerank...). Nothing is saved yet.
	if err := f.searches.SetStatus(ctx, s.ID, models.SearchStatusRanking); err != nil {
		return o, err
	}
	scoreCtx, cancel := context.WithTimeout(ctx, f.cfg.ScoreTimeout)
	defer cancel()
	res, err := f.ai.Score(scoreCtx, aiservice.ScoreRequest{
		Profile:         json.RawMessage(p.Analysis),
		CVText:          p.RawResume,
		DesiredTitle:    s.DesiredTitle,
		DesiredLocation: s.Location,
		Jobs:            toScoreJobs(crawled),
		SearchID:        s.ID,
		Keyword:         s.Keyword,
		ExpFilter:       s.ExpFilter,
	})
	if err != nil {
		if errors.Is(scoreCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return o, fmt.Errorf("%s (%s, %d jobs): %w", ScoreTimeoutError, f.cfg.ScoreTimeout, len(crawled), err)
		}
		return o, fmt.Errorf("score jobs: %w", err)
	}
	o.summary.Rejected, o.summary.Scored = res.Summary.Rejected, res.Summary.Scored
	o.summary.RejectReasons, o.summary.Report = res.Summary.RejectReasons, res.Report

	// 3) Save only the matched jobs, with their analysis: ai_matches references jobs.id
	byUniqueID := make(map[string]services.AnalyzedJob, len(crawled))
	for _, j := range crawled {
		byUniqueID[uniqueID(j)] = j
	}
	var keep []services.AnalyzedJob
	var passed []aiservice.ScoredJob
	for _, r := range res.Results {
		j, ok := byUniqueID[r.JobID]
		if !r.Passed || r.JobData == nil || !ok {
			continue
		}
		keep = append(keep, withAnalysis(j, r.JobData))
		passed = append(passed, r)
	}
	if len(keep) == 0 {
		return o, nil
	}
	if _, err := f.importFn(ctx, services.JobFile{Jobs: keep}); err != nil {
		return o, err
	}

	// 4) Save the matches
	uniqueIDs := make([]string, len(passed))
	for i, r := range passed {
		uniqueIDs[i] = r.JobID
	}
	ids, err := f.jobIDs.IDsByUniqueIDs(ctx, uniqueIDs)
	if err != nil {
		return o, err
	}
	rows := make([]models.AIMatch, 0, len(passed))
	perSource := make(map[string]int)
	for _, r := range passed {
		jobID, ok := ids[r.JobID]
		if !ok {
			continue // rejected by the import validation (no title, no URL...)
		}
		details, err := json.Marshal(matchDetails{
			Penalty: r.Penalty, Components: r.Components,
			MatchedSkills: r.MatchedSkills, MissingSkills: r.MissingSkills,
		})
		if err != nil {
			return o, fmt.Errorf("encode match details: %w", err)
		}
		rows = append(rows, models.AIMatch{
			JobID: jobID, UserID: s.UserID, Score: r.Score, Status: models.AIMatchStatusMatched, SearchID: &s.ID,
			Rank: len(rows) + 1, Details: datatypes.JSON(details), ModelVersion: res.ModelVersion,
		})
		perSource[strings.SplitN(r.JobID, ":", 2)[0]]++
	}
	if err := f.searches.SaveMatches(ctx, rows); err != nil {
		return o, err
	}
	o.matches = len(rows)
	o.summary.Passed = len(rows)
	for i := range o.summary.Sources {
		o.summary.Sources[i].Passed = perSource[strings.ToLower(o.summary.Sources[i].Source)]
	}
	return o, nil
}

// withAnalysis returns j with the analysis the AI service computed while scoring.
func withAnalysis(j services.AnalyzedJob, d *aiservice.JobData) services.AnalyzedJob {
	j.MinYears, j.RequiredYears = d.MinYears, d.RequiredYears
	j.IsRemote, j.Languages = d.IsRemote, d.Languages
	j.Embedding, j.EmbeddingModel = d.Embedding, d.EmbeddingModel
	if d.Location != "" {
		j.Location = d.Location
	}
	if len(d.Skills) > 0 {
		j.Skills = d.Skills
	}
	return j
}

// uniqueID is the Job.UniqueIdentifier the import gives j, which is also the
// job_id the AI service returns.
func uniqueID(j services.AnalyzedJob) string {
	return strings.ToLower(strings.TrimSpace(j.Source)) + ":" + strings.TrimSpace(j.ID)
}

// matchDetails is stored in ai_matches.details.
type matchDetails struct {
	Penalty       float64             `json:"penalty"`
	Components    map[string]*float64 `json:"components"`
	MatchedSkills []string            `json:"matched_skills"`
	MissingSkills []string            `json:"missing_skills"`
}

// finish writes the final state with its own context: the search context may
// already be cancelled by Shutdown.
func (f *FindJobs) finish(id uint, status, errMsg string, o outcome) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	summary, err := json.Marshal(o.summary)
	if err != nil {
		log.Printf("find job %d: encode summary: %v", id, err)
		summary = nil
	}
	errMsg = truncateRunes(strings.ToValidUTF8(errMsg, ""), maxErrorLen)
	if err := f.searches.Finish(ctx, id, status, errMsg, o.summary.Crawled, o.matches, summary); err != nil {
		log.Printf("find job %d: %v", id, err)
	}
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func toScoreJobs(in []services.AnalyzedJob) []aiservice.ScoreJob {
	out := make([]aiservice.ScoreJob, len(in))
	for i, j := range in {
		out[i] = aiservice.ScoreJob{
			ID: j.ID, Source: j.Source, URL: j.URL, Title: j.Title, Company: j.Company,
			Description: j.Description, Requirements: j.Requirements, Skills: j.Skills,
			Location: j.Location, Experience: j.Experience, Salary: j.Salary, Deadline: j.Deadline,
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
