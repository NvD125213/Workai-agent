package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"notification-job-mb/internal/models"
)

// ErrResumeChanged is returned when an analysis is saved for a CV that has
// been replaced in the meantime.
var ErrResumeChanged = errors.New("repository: resume changed since it was analyzed")

type ProfileRepository struct {
	db *gorm.DB
}

func NewProfileRepository(db *gorm.DB) *ProfileRepository {
	return &ProfileRepository{db: db}
}

func (r *ProfileRepository) FindByUserID(ctx context.Context, userID uint) (*models.UserProfile, error) {
	var p models.UserProfile
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find profile of user %d: %w", userID, err)
	}
	return &p, nil
}

// Skills returns the skills of profileID.
func (r *ProfileRepository) Skills(ctx context.Context, profileID uint) ([]models.UserSkill, error) {
	var out []models.UserSkill
	if err := r.db.WithContext(ctx).Where("profile_id = ?", profileID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("list skills of profile %d: %w", profileID, err)
	}
	return out, nil
}

// SaveResume stores the text of a newly uploaded CV. The previous analysis
// belongs to the old CV, so it is cleared together with the user's skills.
func (r *ProfileRepository) SaveResume(ctx context.Context, userID uint, rawResume, resumeHash string) (*models.UserProfile, error) {
	p := models.UserProfile{UserID: userID, RawResume: rawResume, ResumeHash: resumeHash}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"raw_resume": rawResume, "resume_hash": resumeHash, "updated_at": time.Now(),
				"exp_years": 0, "current_title": "", "summary": "", "education": "", "location": "",
				"languages": nil, "embedding": nil, "embedding_model": "", "parsed_at": nil,
				"analysis": nil, "search_keyword": "",
			}),
		}).Omit("User", "UserSkills").Create(&p).Error
		if err != nil {
			return fmt.Errorf("save resume of user %d: %w", userID, err)
		}
		if p.ID == 0 { // some drivers return no ID on the update path
			if err := tx.Where("user_id = ?", userID).Take(&p).Error; err != nil {
				return fmt.Errorf("reload profile of user %d: %w", userID, err)
			}
		}
		if err := tx.Where("profile_id = ?", p.ID).Delete(&models.UserSkill{}).Error; err != nil {
			return fmt.Errorf("clear skills of profile %d: %w", p.ID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveAnalysis writes the AI analysis of the CV identified by resumeHash and
// replaces the user's skills. It returns ErrResumeChanged when the stored CV
// is no longer that one.
func (r *ProfileRepository) SaveAnalysis(ctx context.Context, userID uint, resumeHash string,
	analysis *models.UserProfile, skills []models.UserSkill) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.UserProfile{}).
			Where("user_id = ? AND resume_hash = ?", userID, resumeHash).
			Updates(map[string]any{
				"exp_years": analysis.ExpYears, "current_title": analysis.CurrentTitle,
				"summary": analysis.Summary, "education": analysis.Education, "location": analysis.Location,
				"languages": analysis.Languages, "embedding": analysis.Embedding,
				"embedding_model": analysis.EmbeddingModel, "parsed_at": analysis.ParsedAt,
				"analysis": analysis.Analysis, "search_keyword": analysis.SearchKeyword,
			})
		if res.Error != nil {
			return fmt.Errorf("save analysis of user %d: %w", userID, res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrResumeChanged
		}

		var profileID uint
		if err := tx.Model(&models.UserProfile{}).Where("user_id = ?", userID).Pluck("id", &profileID).Error; err != nil {
			return fmt.Errorf("find profile of user %d: %w", userID, err)
		}
		if err := tx.Where("profile_id = ?", profileID).Delete(&models.UserSkill{}).Error; err != nil {
			return fmt.Errorf("clear skills of profile %d: %w", profileID, err)
		}
		if len(skills) == 0 {
			return nil
		}
		for i := range skills {
			skills[i].ProfileID = profileID
		}
		if err := tx.Create(&skills).Error; err != nil {
			return fmt.Errorf("insert skills of profile %d: %w", profileID, err)
		}
		return nil
	})
}
