package database

import (
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"notification-job-mb/internal/config"
	"notification-job-mb/internal/models"
)

func Connect(cfg config.DBConfig) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("unwrap sql db: %w", err)
	}

	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return db, nil
}

// Migrate bật extension pgvector, chạy GORM AutoMigrate cho toàn bộ model
// domain rồi tạo các index mà GORM không tự sinh được (HNSW cho vector).
func Migrate(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	// Cột vector(1024) cần extension có sẵn trước khi AutoMigrate tạo bảng.
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("create extension vector: %w", err)
	}

	if err := db.AutoMigrate(
		&models.Source{},
		&models.User{},
		&models.Job{},
		&models.RefreshToken{},
		&models.PasswordResetOTP{},
		&models.UserProfile{},
		&models.JobSkill{},
		&models.UserSkill{},
		&models.CVAnalysis{},
		&models.SkillAlias{},
		&models.JobSearch{},
		&models.SearchJob{},
		&models.AIMatch{},
		&models.Notification{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	// Tìm job gần nhất theo cosine distance (embedding <=> ?).
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_embedding
		ON jobs USING hnsw (embedding vector_cosine_ops)`).Error; err != nil {
		return fmt.Errorf("create index idx_jobs_embedding: %w", err)
	}

	// Mỗi người dùng chỉ có một lượt tìm việc đang chạy; hai request đồng thời thì
	// lượt thứ hai vi phạm index này thay vì cùng lọt qua một bước kiểm tra trước.
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_job_searches_active_user
		ON job_searches (user_id) WHERE status IN ('QUEUED', 'CRAWLING', 'ANALYZING', 'RANKING')`).Error; err != nil {
		return fmt.Errorf("create index idx_job_searches_active_user: %w", err)
	}

	// Tương tự: mỗi người dùng chỉ có một lượt phân tích CV đang chạy.
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_cv_analyses_active_user
		ON cv_analyses (user_id) WHERE status IN ('QUEUED', 'ANALYZING')`).Error; err != nil {
		return fmt.Errorf("create index idx_cv_analyses_active_user: %w", err)
	}

	return nil
}

func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("unwrap sql db: %w", err)
	}

	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("close postgres: %w", err)
	}

	return nil
}
