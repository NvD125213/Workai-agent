package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/datatypes"

	aiservice "notification-job-mb/internal/adapter/ai-service"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

var (
	// ErrCVNotUploaded: analyze was called before any CV was uploaded.
	ErrCVNotUploaded = errors.New("cv: no CV uploaded yet")
	// ErrCVChanged: a new CV was uploaded while the previous one was being analyzed.
	ErrCVChanged = errors.New("cv: a newer CV was uploaded during the analysis")
	// ErrAIUnavailable: the AI service is down, timed out or failed (not the CV's fault).
	ErrAIUnavailable = errors.New("cv: AI service unavailable")
	// ErrAnalysisInProgress: the user already has an unfinished analysis.
	ErrAnalysisInProgress = repositories.ErrAnalysisInProgress
	// ErrShuttingDown: the server is stopping and accepts no new analysis.
	ErrShuttingDown = errors.New("cv: server is shutting down")
)

// CVRejectedError is the AI service refusing the CV (unsupported format,
// scanned image without text...). Message is meant for the user.
type CVRejectedError struct {
	Message string
}

func (e *CVRejectedError) Error() string { return "cv rejected: " + e.Message }

const (
	parseTimeout   = 2 * time.Minute
	analyzeTimeout = 6 * time.Minute // a local LLM takes 30 s to a few minutes per CV
	maxSkillLevel  = 50              // user_skills.level
)

type cvAI interface {
	ParseCV(ctx context.Context, filename string, file io.Reader) (string, error)
	AnalyzeCV(ctx context.Context, cvText string) (*aiservice.Analysis, error)
}

type profileStore interface {
	FindByUserID(ctx context.Context, userID uint) (*models.UserProfile, error)
	Skills(ctx context.Context, profileID uint) ([]models.UserSkill, error)
	SaveResume(ctx context.Context, userID uint, rawResume, resumeHash string) (*models.UserProfile, error)
	SaveAnalysis(ctx context.Context, userID uint, resumeHash string, analysis *models.UserProfile, skills []models.UserSkill) error
}

type analysisStore interface {
	Create(ctx context.Context, a *models.CVAnalysis) error
	SetStatus(ctx context.Context, id uint, status string) error
	Finish(ctx context.Context, id uint, status, errMsg string) error
	FindForUser(ctx context.Context, userID, id uint) (*models.CVAnalysis, error)
}

// CVService parses CVs and analyzes them in the background: an analysis takes
// 30 s to a few minutes, so POST /analyze-cv returns at once and its progress
// is visible through the cv_analyses row: QUEUED -> ANALYZING -> DONE / FAILED.
type CVService struct {
	ai       cvAI
	profiles profileStore
	analyses analysisStore

	ctx    context.Context // cancelled by Shutdown; parent of every running analysis
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewCVService(ai cvAI, profiles profileStore, analyses analysisStore) *CVService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CVService{ai: ai, profiles: profiles, analyses: analyses, ctx: ctx, cancel: cancel}
}

// ParseCV extracts the text of an uploaded CV and stores it as the user's
// current CV. The previous analysis is discarded: it described the old CV.
func (s *CVService) ParseCV(ctx context.Context, userID uint, filename string, file io.Reader) (*models.UserProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, parseTimeout)
	defer cancel()

	text, err := s.ai.ParseCV(ctx, filename, file)
	if err != nil {
		return nil, aiError("parse cv", err)
	}
	sum := sha256.Sum256([]byte(text))
	return s.profiles.SaveResume(ctx, userID, text, hex.EncodeToString(sum[:]))
}

// StartAnalysis queues the analysis of the user's current CV and returns it at
// once; the AI work runs in the background. Follow it with GetAnalysis.
func (s *CVService) StartAnalysis(ctx context.Context, userID uint) (*models.CVAnalysis, error) {
	if s.ctx.Err() != nil {
		return nil, ErrShuttingDown
	}
	p, err := s.profiles.FindByUserID(ctx, userID)
	if errors.Is(err, repositories.ErrNotFound) || (err == nil && p.RawResume == "") {
		return nil, ErrCVNotUploaded
	}
	if err != nil {
		return nil, err
	}
	a := &models.CVAnalysis{
		UserID: userID, ProfileID: p.ID, ResumeHash: p.ResumeHash, Status: models.AnalysisStatusQueued,
	}
	if err := s.analyses.Create(ctx, a); err != nil {
		return nil, err
	}
	s.wg.Go(func() { s.runAnalysis(a, p) })
	return a, nil
}

// AnalysisView is an analysis with, once DONE, the profile it produced.
type AnalysisView struct {
	Analysis *models.CVAnalysis
	// Outdated: a newer CV was uploaded after this analysis; its result is gone.
	Outdated bool
	// Set when the analysis is DONE and not Outdated.
	Profile *models.UserProfile
	Skills  []models.UserSkill
	// Raw is the full analysis of the AI service (years as a decimal, all job titles, experiences...).
	Raw []byte
}

// GetAnalysis returns the analysis id of userID; repositories.ErrNotFound when
// it does not exist or belongs to someone else.
func (s *CVService) GetAnalysis(ctx context.Context, userID, id uint) (*AnalysisView, error) {
	a, err := s.analyses.FindForUser(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	view := &AnalysisView{Analysis: a}
	if a.Status != models.AnalysisStatusDone {
		return view, nil
	}
	p, err := s.profiles.FindByUserID(ctx, userID)
	if err != nil && !errors.Is(err, repositories.ErrNotFound) {
		return nil, err
	}
	if p == nil || p.ResumeHash != a.ResumeHash || p.ParsedAt == nil {
		view.Outdated = true
		return view, nil
	}
	if view.Skills, err = s.profiles.Skills(ctx, p.ID); err != nil {
		return nil, err
	}
	view.Profile, view.Raw = p, p.Analysis
	return view, nil
}

// Shutdown stops accepting analyses, cancels the running ones (they end as
// FAILED) and waits for them until ctx ends.
func (s *CVService) Shutdown(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *CVService) runAnalysis(a *models.CVAnalysis, p *models.UserProfile) {
	status, msg := models.AnalysisStatusDone, ""
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cv analysis %d: panic: %v", a.ID, r)
			status, msg = models.AnalysisStatusFailed, "internal error"
		}
		// Own context: the service context may already be cancelled by Shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.analyses.Finish(ctx, a.ID, status, msg); err != nil {
			log.Printf("cv analysis %d: %v", a.ID, err)
		}
	}()

	err := s.analyses.SetStatus(s.ctx, a.ID, models.AnalysisStatusAnalyzing)
	if err == nil {
		err = s.analyze(s.ctx, a.UserID, p)
	}
	if err != nil {
		log.Printf("cv analysis %d: %v", a.ID, err)
		status, msg = models.AnalysisStatusFailed, analysisFailure(err, s.ctx.Err() != nil)
		return
	}
	log.Printf("cv analysis %d: done", a.ID)
}

// analysisFailure is the message shown to the user for a failed analysis.
func analysisFailure(err error, stopping bool) string {
	var rejected *CVRejectedError
	switch {
	case stopping:
		return "server stopped during the analysis; analyze again"
	case errors.As(err, &rejected):
		return rejected.Message
	case errors.Is(err, ErrCVChanged):
		return "a newer CV was uploaded during the analysis; analyze again"
	case errors.Is(err, ErrAIUnavailable):
		return "the AI service failed; try again later"
	default:
		return "internal error"
	}
}

// analyze analyzes the CV of p with the AI service, then stores the
// structured profile, its embedding and the search keyword.
func (s *CVService) analyze(ctx context.Context, userID uint, p *models.UserProfile) error {
	ctx, cancel := context.WithTimeout(ctx, analyzeTimeout)
	defer cancel()
	a, err := s.ai.AnalyzeCV(ctx, p.RawResume)
	if err != nil {
		return aiError("analyze cv", err)
	}

	now := time.Now()
	vec := pgvector.NewVector(a.Embedding)
	p.ExpYears = a.UserProfile.ExpYears
	p.CurrentTitle = truncate(a.UserProfile.CurrentTitle, maxTitleLen)
	p.Summary = a.UserProfile.Summary
	p.Education = a.UserProfile.Education
	p.Location = truncate(a.UserProfile.Location, 100)
	p.Languages = datatypes.JSON(a.UserProfile.Languages)
	p.Embedding = &vec
	p.EmbeddingModel = a.EmbeddingModel
	p.ParsedAt = &now
	p.Analysis = datatypes.JSON(a.Profile)
	p.SearchKeyword = truncate(a.SearchKeyword, 255)

	skills := make([]models.UserSkill, 0, len(a.UserSkills))
	for _, sk := range a.UserSkills {
		if sk.SkillName == "" {
			continue
		}
		skills = append(skills, models.UserSkill{
			SkillName: truncate(sk.SkillName, maxSkillLen), Level: truncate(sk.Level, maxSkillLevel),
		})
	}
	// Bound by the stored hash, not the AI's: a CV replaced meanwhile must not get this analysis.
	if err := s.profiles.SaveAnalysis(ctx, userID, p.ResumeHash, p, skills); err != nil {
		if errors.Is(err, repositories.ErrResumeChanged) {
			return ErrCVChanged
		}
		return err
	}
	return nil
}

// aiError turns an AI service refusal of the input into CVRejectedError and
// any other AI failure into ErrAIUnavailable.
func aiError(op string, err error) error {
	var e *aiservice.Error
	if aiservice.IsInputError(err) && errors.As(err, &e) {
		return &CVRejectedError{Message: e.Detail}
	}
	if errors.Is(err, context.Canceled) {
		return err // the client went away
	}
	return fmt.Errorf("%s: %w: %w", op, ErrAIUnavailable, err)
}
