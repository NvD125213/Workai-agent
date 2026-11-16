// Package models holds GORM domain entities shared by the API and worker.
package models

import (
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/datatypes"
)

// ── Roles ───────────────────────────────────────────────────────────────────

const (
	RoleUser  = "USER"
	RoleAdmin = "ADMIN"
)

// ── Job statuses ────────────────────────────────────────────────────────────

const (
	JobStatusPending     = "PENDING"
	JobStatusProcessing  = "PROCESSING"
	JobStatusAIProcessed = "AI_PROCESSED"
	JobStatusFailed      = "FAILED"
)

// ── AI match statuses ───────────────────────────────────────────────────────

const (
	AIMatchStatusMatched  = "MATCHED"
	AIMatchStatusNotified = "NOTIFIED"
	AIMatchStatusSkipped  = "SKIPPED"
)

// ── Notification statuses ───────────────────────────────────────────────────

const (
	NotificationStatusSuccess = "SUCCESS"
	NotificationStatusFailed  = "FAILED"
)

// ── Skill levels ────────────────────────────────────────────────────────────

const (
	SkillLevelJunior = "JUNIOR"
	SkillLevelMid    = "MID"
	SkillLevelSenior = "SENIOR"
)

const (
	SearchStatusQueued    = "QUEUED"
	SearchStatusCrawling  = "CRAWLING"
	SearchStatusAnalyzing = "ANALYZING"
	SearchStatusRanking   = "RANKING"
	SearchStatusDone      = "DONE"
	SearchStatusFailed    = "FAILED"
)

// 1. NHÓM QUẢN LÝ NGUỒN VÀ DỮ LIỆU CÀO (INGESTION LAYER)

type Source struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Name      string    `gorm:"type:varchar(100);not null;uniqueIndex" json:"name"`
	URL       string    `gorm:"type:varchar(255);not null" json:"url"`
	IsActive  bool      `gorm:"not null;default:true;index" json:"is_active"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime" json:"created_at"`

	Jobs []Job `gorm:"foreignKey:SourceID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"jobs,omitempty"`
}

type Job struct {
	ID               uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	SourceID         *uint      `gorm:"index" json:"source_id"`
	UniqueIdentifier string     `gorm:"type:varchar(255);not null;uniqueIndex" json:"unique_identifier"`
	Title            string     `gorm:"type:varchar(255);not null" json:"title"`
	Company          string     `gorm:"type:varchar(150)" json:"company"`
	Description      string     `gorm:"type:text;not null" json:"description"`
	URL              string     `gorm:"type:text;not null" json:"url"`
	RawSalary        string     `gorm:"type:varchar(100)" json:"raw_salary"`
	Status           string     `gorm:"type:varchar(50);not null;default:'PENDING';index" json:"status"`
	PostedAt         *time.Time `json:"posted_at,omitempty"`
	CreatedAt        time.Time  `gorm:"not null;autoCreateTime" json:"created_at"`

	Source    *Source    `gorm:"foreignKey:SourceID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"source,omitempty"`
	JobSkills []JobSkill `gorm:"foreignKey:JobID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"job_skills,omitempty"`
	AIMatches []AIMatch  `gorm:"foreignKey:JobID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"ai_matches,omitempty"`

	// Do dịch vụ AI điền khi phân tích job
	Location       string           `gorm:"type:varchar(100)" json:"location"`
	IsRemote       bool             `gorm:"not null;default:false" json:"is_remote"`
	MinYears       *int             `json:"min_years"`      // nil = không ghi yêu cầu, khác với 0
	RequiredYears  *int             `json:"required_years"` // max(MinYears, sàn theo cấp bậc trong tiêu đề); FindCandidateJobs lọc theo cột này
	Languages      datatypes.JSON   `gorm:"type:jsonb" json:"languages"`
	Embedding      *pgvector.Vector `gorm:"type:vector(1024)" json:"-"`
	EmbeddingModel string           `gorm:"type:varchar(100)" json:"-"`
	AnalyzedAt     *time.Time       `json:"analyzed_at,omitempty"`
	AIAttempts     int              `gorm:"not null;default:0" json:"-"`
	AIError        string           `gorm:"type:text" json:"-"`
}

type JobSkill struct {
	ID        uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	JobID     uint   `gorm:"not null;uniqueIndex:idx_job_skill;index" json:"job_id"`
	SkillName string `gorm:"type:varchar(100);not null;uniqueIndex:idx_job_skill" json:"skill_name"`

	Job *Job `gorm:"foreignKey:JobID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

// 2. NHÓM NGƯỜI DÙNG VÀ HỒ SƠ (USER & PROFILE LAYER)

type User struct {
	ID            uint    `gorm:"primaryKey;autoIncrement" json:"id"`
	Email         string  `gorm:"type:varchar(150);not null;uniqueIndex" json:"email"`
	Name          string  `gorm:"type:varchar(100);not null" json:"name"`
	PasswordHash  string  `gorm:"type:varchar(255);not null" json:"-"`
	Role          string  `gorm:"type:varchar(50);not null;default:'USER';index" json:"role"`
	IsActive      bool    `gorm:"not null;default:true;index" json:"is_active"`
	EmailVerified bool    `gorm:"not null;default:false" json:"email_verified"`
	FCMToken      *string `gorm:"type:varchar(255);index" json:"-"`
	// TelegramID: id Telegram của người đăng ký qua bot; NULL với tài khoản tạo cách khác.
	// Tài khoản đăng ký qua Telegram có PasswordHash rỗng nên không đăng nhập bằng mật khẩu được.
	TelegramID *int64    `gorm:"uniqueIndex" json:"telegram_id,omitempty"`
	CreatedAt  time.Time `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time `gorm:"not null;autoUpdateTime" json:"updated_at"`

	UserProfile   *UserProfile   `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"profile,omitempty"`
	RefreshTokens []RefreshToken `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	AIMatches     []AIMatch      `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"ai_matches,omitempty"`
}

type RefreshToken struct {
	ID        uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    uint       `gorm:"not null;index" json:"user_id"`
	TokenHash string     `gorm:"type:varchar(255);not null;uniqueIndex" json:"-"`
	ExpiresAt time.Time  `gorm:"not null;index" json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `gorm:"not null;autoCreateTime" json:"created_at"`
	UserAgent string     `gorm:"type:text" json:"user_agent,omitempty"`
	IPAddress string     `gorm:"type:varchar(100)" json:"ip_address,omitempty"`

	User *User `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

// PasswordResetOTP is a one-time code emailed to a user to authorize a password
// reset. Only an HMAC of the code is stored.
//
// Verifying the code consumes it and attaches a short-lived reset token to the
// row; the token (stored as a SHA-256 hash) is then exchanged once for a new
// password. ResetTokenHash is cleared when the token is used or superseded.
type PasswordResetOTP struct {
	ID         uint       `gorm:"primaryKey;autoIncrement"`
	UserID     uint       `gorm:"not null;index:idx_otp_user_created,priority:1"`
	CodeHash   string     `gorm:"type:varchar(64);not null"`
	Attempts   int        `gorm:"not null;default:0"`
	ExpiresAt  time.Time  `gorm:"not null;index"`
	ConsumedAt *time.Time `gorm:"index"`
	CreatedAt  time.Time  `gorm:"not null;index:idx_otp_user_created,priority:2"`

	ResetTokenHash      *string `gorm:"type:varchar(64);uniqueIndex"`
	ResetTokenExpiresAt *time.Time

	User *User `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

type UserProfile struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    uint      `gorm:"not null;uniqueIndex" json:"user_id"`
	RawResume string    `gorm:"type:text" json:"raw_resume"`
	ExpYears  int       `gorm:"not null;default:0" json:"exp_years"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime" json:"updated_at"`

	User       *User       `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	UserSkills []UserSkill `gorm:"foreignKey:ProfileID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"user_skills,omitempty"`

	ResumeHash     string           `gorm:"type:varchar(64)" json:"-"` // SHA-256 của raw_resume
	CurrentTitle   string           `gorm:"type:varchar(255)" json:"current_title"`
	Summary        string           `gorm:"type:text" json:"summary"`
	Education      string           `gorm:"type:text" json:"education"`
	Location       string           `gorm:"type:varchar(100)" json:"location"`
	Languages      datatypes.JSON   `gorm:"type:jsonb" json:"languages"`
	Embedding      *pgvector.Vector `gorm:"type:vector(1024)" json:"-"`
	EmbeddingModel string           `gorm:"type:varchar(100)" json:"-"`
	ParsedAt       *time.Time       `json:"parsed_at,omitempty"`
	Analysis       datatypes.JSON   `gorm:"type:jsonb" json:"-"`                     // hồ sơ đầy đủ từ AI (số năm lẻ, các chức danh...), gửi lại khi chấm điểm
	SearchKeyword  string           `gorm:"type:varchar(255)" json:"search_keyword"` // keyword nghĩa rộng để cào job, vd "Developer"
}

const (
	AnalysisStatusQueued    = "QUEUED"
	AnalysisStatusAnalyzing = "ANALYZING"
	AnalysisStatusDone      = "DONE"
	AnalysisStatusFailed    = "FAILED"
)

// CVAnalysis là một lần người dùng yêu cầu phân tích CV (chạy nền). Kết quả ghi vào user_profiles;
// find-job nhận ID của lượt phân tích này để biết đang tìm việc cho CV nào.
type CVAnalysis struct {
	ID         uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID     uint       `gorm:"not null;index" json:"user_id"`
	ProfileID  uint       `gorm:"not null;index" json:"profile_id"`
	ResumeHash string     `gorm:"type:varchar(64);not null" json:"-"` // CV được phân tích; khác user_profiles.resume_hash = đã có CV mới
	Status     string     `gorm:"type:varchar(50);not null;default:'QUEUED';index" json:"status"`
	Error      string     `gorm:"type:text" json:"error,omitempty"`
	CreatedAt  time.Time  `gorm:"not null;autoCreateTime" json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	User    *User        `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	Profile *UserProfile `gorm:"foreignKey:ProfileID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

type UserSkill struct {
	ID        uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	ProfileID uint   `gorm:"not null;uniqueIndex:idx_user_profile_skill;index" json:"profile_id"`
	SkillName string `gorm:"type:varchar(100);not null;uniqueIndex:idx_user_profile_skill" json:"skill_name"`
	Level     string `gorm:"type:varchar(50)" json:"level"`

	UserProfile *UserProfile `gorm:"foreignKey:ProfileID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

// 3. NHÓM KẾT QUẢ AI & THÔNG BÁO (AI AGENT & NOTIFICATION LAYER)

type AIMatch struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	JobID     uint      `gorm:"not null;uniqueIndex:idx_job_user_match;index" json:"job_id"`
	UserID    uint      `gorm:"not null;uniqueIndex:idx_job_user_match;index" json:"user_id"`
	Score     float64   `gorm:"type:decimal(5,2);not null" json:"score"`
	Reasoning string    `gorm:"type:text" json:"reasoning"`
	Status    string    `gorm:"type:varchar(50);not null;default:'MATCHED';index" json:"status"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime" json:"created_at"`

	Job           *Job           `gorm:"foreignKey:JobID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"job,omitempty"`
	User          *User          `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"user,omitempty"`
	Notifications []Notification `gorm:"foreignKey:MatchID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"notifications,omitempty"`

	SearchID     *uint          `gorm:"index" json:"search_id"`
	Rank         int            `gorm:"not null;default:0" json:"rank"`
	Details      datatypes.JSON `gorm:"type:jsonb" json:"details"` // điểm thành phần, kỹ năng khớp/thiếu
	ModelVersion string         `gorm:"type:varchar(100)" json:"model_version"`

	Search *JobSearch `gorm:"foreignKey:SearchID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"-"`
}

// Notification gắn với một lượt tìm (SearchID, "Tìm thấy 8 việc phù hợp với ...")
// hoặc với một match cụ thể (MatchID). Cả hai đều có thể rỗng.
type Notification struct {
	ID       uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	MatchID  *uint     `gorm:"index" json:"match_id"`
	SearchID *uint     `gorm:"index" json:"search_id"`
	Title    string    `gorm:"type:varchar(255);not null" json:"title"`
	Body     string    `gorm:"type:text;not null" json:"body"`
	SentAt   time.Time `gorm:"not null;autoCreateTime" json:"sent_at"`
	Status   string    `gorm:"type:varchar(50);not null;default:'SUCCESS';index" json:"status"`

	AIMatch *AIMatch   `gorm:"foreignKey:MatchID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	Search  *JobSearch `gorm:"foreignKey:SearchID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

// JobSearch là một lần người dùng yêu cầu tìm việc.
type JobSearch struct {
	ID           uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID       uint   `gorm:"not null;index" json:"user_id"`
	AnalysisID   *uint  `gorm:"index" json:"analysis_id"`                 // lượt phân tích CV dùng để tìm
	ExpFilter    bool   `gorm:"not null;default:false" json:"exp_filter"` // lọc theo khoảng kinh nghiệm, xếp hạng theo lương
	DesiredTitle string `gorm:"type:varchar(255);not null" json:"desired_title"`
	Keyword      string `gorm:"type:varchar(255)" json:"keyword"` // keyword đã dùng để cào
	Location     string `gorm:"type:varchar(100)" json:"location"`
	RemoteOK     bool   `gorm:"not null;default:false" json:"remote_ok"`
	Status       string `gorm:"type:varchar(50);not null;default:'QUEUED';index" json:"status"`
	Error        string `gorm:"type:text" json:"error,omitempty"`
	JobsFound    int    `gorm:"not null;default:0" json:"jobs_found"`
	MatchesFound int    `gorm:"not null;default:0" json:"matches_found"`
	// Số job theo từng bước (đã cào, bị loại, được chấm, đạt), theo nguồn, các lý do loại phổ biến.
	// Chỉ job đạt mới được lưu vào jobs, nên đây là nơi duy nhất còn dấu vết của job bị bỏ.
	Summary    datatypes.JSON `gorm:"type:jsonb" json:"summary"`
	CreatedAt  time.Time      `gorm:"not null;autoCreateTime" json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`

	User     *User       `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	Analysis *CVAnalysis `gorm:"foreignKey:AnalysisID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"-"`
}

// SearchJob ghi nhận job nào được cào ra trong lượt tìm nào.
type SearchJob struct {
	SearchID uint `gorm:"primaryKey" json:"search_id"`
	JobID    uint `gorm:"primaryKey;index" json:"job_id"`

	Search *JobSearch `gorm:"foreignKey:SearchID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	Job    *Job       `gorm:"foreignKey:JobID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

type SkillAlias struct {
	Alias     string `gorm:"type:varchar(100);primaryKey" json:"alias"`         // "reactjs"
	Canonical string `gorm:"type:varchar(100);not null;index" json:"canonical"` // "react"
}
