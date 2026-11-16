package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"notification-job-mb/internal/models"
)

// ErrSearchInProgress is returned when the user already has an unfinished search.
var ErrSearchInProgress = errors.New("repository: a job search is already in progress")

// activeSearchStatuses must match the WHERE of idx_job_searches_active_user.
var activeSearchStatuses = []string{
	models.SearchStatusQueued, models.SearchStatusCrawling, models.SearchStatusAnalyzing, models.SearchStatusRanking,
}

type SearchRepository struct {
	db *gorm.DB
}

func NewSearchRepository(db *gorm.DB) *SearchRepository {
	return &SearchRepository{db: db}
}

// Create inserts a QUEUED search. idx_job_searches_active_user arbitrates
// concurrent requests: the loser gets ErrSearchInProgress.
func (r *SearchRepository) Create(ctx context.Context, s *models.JobSearch) error {
	err := r.db.WithContext(ctx).Omit("User", "Analysis").Create(s).Error
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_job_searches_active_user" {
		return ErrSearchInProgress
	}
	if err != nil {
		return fmt.Errorf("create job search: %w", err)
	}
	return nil
}

func (r *SearchRepository) SetStatus(ctx context.Context, id uint, status string) error {
	err := r.db.WithContext(ctx).Model(&models.JobSearch{}).Where("id = ?", id).Update("status", status).Error
	if err != nil {
		return fmt.Errorf("set status of job search %d: %w", id, err)
	}
	return nil
}

// Finish moves the search to DONE or FAILED with its counters, summary and error message.
func (r *SearchRepository) Finish(ctx context.Context, id uint, status, errMsg string, jobsFound, matchesFound int,
	summary datatypes.JSON) error {
	err := r.db.WithContext(ctx).Model(&models.JobSearch{}).Where("id = ?", id).Updates(map[string]any{
		"status": status, "error": errMsg, "jobs_found": jobsFound, "matches_found": matchesFound,
		"summary": summary, "finished_at": time.Now(),
	}).Error
	if err != nil {
		return fmt.Errorf("finish job search %d: %w", id, err)
	}
	return nil
}

// FailUnfinished marks searches left unfinished by a previous process as FAILED.
func (r *SearchRepository) FailUnfinished(ctx context.Context, errMsg string) (int64, error) {
	res := r.db.WithContext(ctx).Model(&models.JobSearch{}).Where("status IN ?", activeSearchStatuses).
		Updates(map[string]any{"status": models.SearchStatusFailed, "error": errMsg, "finished_at": time.Now()})
	if res.Error != nil {
		return 0, fmt.Errorf("fail unfinished job searches: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// FindForUser returns the search id when it belongs to userID, ErrNotFound otherwise.
func (r *SearchRepository) FindForUser(ctx context.Context, userID, id uint) (*models.JobSearch, error) {
	var s models.JobSearch
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find job search %d: %w", id, err)
	}
	return &s, nil
}

// SaveMatches upserts matches by (job_id, user_id): searching again updates the
// earlier match of the same job instead of failing on idx_job_user_match.
func (r *SearchRepository) SaveMatches(ctx context.Context, matches []models.AIMatch) error {
	if len(matches) == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "job_id"}, {Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"score", "reasoning", "status", "search_id", "rank", "details", "model_version",
		}),
	}).Omit("Job", "User", "Notifications", "Search").Create(&matches).Error
	if err != nil {
		return fmt.Errorf("save matches: %w", err)
	}
	return nil
}

// ListMatches returns the matches of a search, best first, with their job.
func (r *SearchRepository) ListMatches(ctx context.Context, searchID uint) ([]models.AIMatch, error) {
	var out []models.AIMatch
	err := r.db.WithContext(ctx).
		Preload("Job", func(db *gorm.DB) *gorm.DB {
			return db.Select("id", "source_id", "unique_identifier", "title", "company", "url", "raw_salary", "posted_at")
		}).
		Preload("Job.Source", func(db *gorm.DB) *gorm.DB { return db.Select("id", "name") }).
		Where("search_id = ?", searchID).Order("rank").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list matches of job search %d: %w", searchID, err)
	}
	return out, nil
}
