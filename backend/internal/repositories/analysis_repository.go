package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"notification-job-mb/internal/models"
)

// ErrAnalysisInProgress is returned when the user already has an unfinished CV analysis.
var ErrAnalysisInProgress = errors.New("repository: a CV analysis is already in progress")

type AnalysisRepository struct {
	db *gorm.DB
}

func NewAnalysisRepository(db *gorm.DB) *AnalysisRepository {
	return &AnalysisRepository{db: db}
}

// Create inserts a QUEUED analysis. idx_cv_analyses_active_user arbitrates
// concurrent requests: the loser gets ErrAnalysisInProgress.
func (r *AnalysisRepository) Create(ctx context.Context, a *models.CVAnalysis) error {
	err := r.db.WithContext(ctx).Omit("User", "Profile").Create(a).Error
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_cv_analyses_active_user" {
		return ErrAnalysisInProgress
	}
	if err != nil {
		return fmt.Errorf("create cv analysis: %w", err)
	}
	return nil
}

func (r *AnalysisRepository) SetStatus(ctx context.Context, id uint, status string) error {
	err := r.db.WithContext(ctx).Model(&models.CVAnalysis{}).Where("id = ?", id).Update("status", status).Error
	if err != nil {
		return fmt.Errorf("set status of cv analysis %d: %w", id, err)
	}
	return nil
}

// Finish moves the analysis to DONE or FAILED with its error message.
func (r *AnalysisRepository) Finish(ctx context.Context, id uint, status, errMsg string) error {
	err := r.db.WithContext(ctx).Model(&models.CVAnalysis{}).Where("id = ?", id).Updates(map[string]any{
		"status": status, "error": errMsg, "finished_at": time.Now(),
	}).Error
	if err != nil {
		return fmt.Errorf("finish cv analysis %d: %w", id, err)
	}
	return nil
}

// FailUnfinished marks analyses left unfinished by a previous process as FAILED.
func (r *AnalysisRepository) FailUnfinished(ctx context.Context, errMsg string) (int64, error) {
	res := r.db.WithContext(ctx).Model(&models.CVAnalysis{}).
		Where("status IN ?", []string{models.AnalysisStatusQueued, models.AnalysisStatusAnalyzing}).
		Updates(map[string]any{"status": models.AnalysisStatusFailed, "error": errMsg, "finished_at": time.Now()})
	if res.Error != nil {
		return 0, fmt.Errorf("fail unfinished cv analyses: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// FindForUser returns the analysis id when it belongs to userID, ErrNotFound otherwise.
func (r *AnalysisRepository) FindForUser(ctx context.Context, userID, id uint) (*models.CVAnalysis, error) {
	var a models.CVAnalysis
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find cv analysis %d: %w", id, err)
	}
	return &a, nil
}
