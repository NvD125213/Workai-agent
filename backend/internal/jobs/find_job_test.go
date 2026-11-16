package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	aiservice "notification-job-mb/internal/adapter/ai-service"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

type fakeAnalyses map[uint]*models.CVAnalysis

func (f fakeAnalyses) FindForUser(_ context.Context, userID, id uint) (*models.CVAnalysis, error) {
	if a, ok := f[id]; ok && a.UserID == userID {
		return a, nil
	}
	return nil, repositories.ErrNotFound
}

type fakeProfiles struct{ p *models.UserProfile }

func (f fakeProfiles) FindByUserID(context.Context, uint) (*models.UserProfile, error) { return f.p, nil }

// noSearches fails Create, so a Start that passes every check stops there.
type noSearches struct{ searchStore }

func (noSearches) Create(context.Context, *models.JobSearch) error { return errors.New("created") }

func TestStartChecksAnalysis(t *testing.T) {
	now := time.Now()
	profile := &models.UserProfile{ID: 1, UserID: 7, ResumeHash: "new", ParsedAt: &now,
		Analysis: datatypes.JSON(`{}`), SearchKeyword: "Content"}
	f := &FindJobs{
		searches: noSearches{},
		profiles: fakeProfiles{profile},
		analyses: fakeAnalyses{
			1: {ID: 1, UserID: 7, ResumeHash: "new", Status: models.AnalysisStatusAnalyzing},
			2: {ID: 2, UserID: 7, ResumeHash: "old", Status: models.AnalysisStatusDone},
			3: {ID: 3, UserID: 8, ResumeHash: "new", Status: models.AnalysisStatusDone},
			4: {ID: 4, UserID: 7, ResumeHash: "new", Status: models.AnalysisStatusDone},
		},
		ctx: context.Background(),
	}
	tests := []struct {
		id   uint
		want error
	}{
		{1, ErrAnalysisNotDone},
		{2, ErrAnalysisOutdated},
		{3, repositories.ErrNotFound}, // someone else's analysis
		{99, repositories.ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := f.Start(context.Background(), 7, StartRequest{AnalysisID: tt.id}); !errors.Is(err, tt.want) {
			t.Errorf("analysis %d: err = %v, want %v", tt.id, err, tt.want)
		}
	}
	// A DONE, current analysis gets through to creating the search.
	if _, err := f.Start(context.Background(), 7, StartRequest{AnalysisID: 4, ExpFilter: true}); err == nil ||
		err.Error() != "created" {
		t.Errorf("analysis 4: err = %v, want the search to be created", err)
	}
}

// statusSearches records nothing; search only needs SetStatus to succeed.
type statusSearches struct{ searchStore }

func (statusSearches) SetStatus(context.Context, uint, string) error { return nil }

// slowScorer answers only when the request's context ends, like an AI service
// still scoring when the timeout hits.
type slowScorer struct{}

func (slowScorer) Score(ctx context.Context, _ aiservice.ScoreRequest) (*aiservice.ScoreResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSearchScoreTimeout(t *testing.T) {
	f := &FindJobs{
		searches: statusSearches{},
		ai:       slowScorer{},
		crawl: func(context.Context, string, int) []SourceResult {
			return []SourceResult{{Source: "topcv", Jobs: []services.AnalyzedJob{{ID: "1", Title: "Content"}}}}
		},
		cfg: FindJobConfig{ScoreTimeout: 20 * time.Millisecond},
		ctx: context.Background(),
	}
	_, err := f.search(&models.JobSearch{ID: 1, Keyword: "Content"}, &models.UserProfile{Analysis: datatypes.JSON(`{}`)})
	if err == nil || !strings.HasPrefix(err.Error(), ScoreTimeoutError) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a %q error", err, ScoreTimeoutError)
	}
}
