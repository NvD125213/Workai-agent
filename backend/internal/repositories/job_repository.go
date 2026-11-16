package repositories

import (
	"context"
	"fmt"
	"strconv"

	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"notification-job-mb/internal/models"
)

// ExpToleranceYears: job cần X năm thì ứng viên phải có ít nhất X - ExpToleranceYears năm.
// Giữ bằng EXP_TOLERANCE_YEARS trong llm/matching/config.py để SQL và Python loại cùng một tập job.
const ExpToleranceYears = 1

const (
	defaultCandidateLimit = 50 // = TOP_K_RETRIEVE bên Python: số job đem đi rerank
	maxCandidateLimit     = 200
	defaultHNSWEfSearch   = 40 // mặc định của pgvector
)

// CandidateQuery mô tả một lần lấy job ứng viên cho một hồ sơ.
type CandidateQuery struct {
	Embedding      pgvector.Vector // embedding của hồ sơ (user_profiles.embedding)
	EmbeddingModel string          // chỉ so với job nhúng bằng cùng mô hình; vector khác mô hình không so được
	// Số năm kinh nghiệm của ứng viên; nil = chưa biết (CV chưa phân tích) thì không lọc kinh nghiệm.
	ExpYears *int
	Limit    int // <= 0 thì dùng 50, tối đa 200
}

// CandidateJob là job kèm cosine similarity với hồ sơ (1 = giống hệt).
// Job chỉ có các cột cần cho chấm điểm và JobSkills; không gồm Embedding.
type CandidateJob struct {
	models.Job
	Cosine float64 `gorm:"column:cosine"`
}

// candidateJobsSQL: bộ lọc cứng nằm trong WHERE, áp dụng ngay trong lúc duyệt index HNSW
// (hnsw.iterative_scan), nên job sai cấp kinh nghiệm không chiếm chỗ trong LIMIT.
// COALESCE: job phân tích trước khi có cột required_years thì dùng tạm min_years.
const candidateJobsSQL = `
SELECT j.id, j.source_id, j.unique_identifier, j.title, j.company, j.description, j.url, j.raw_salary, j.posted_at,
       j.location, j.is_remote, j.min_years, j.required_years, j.languages,
       1 - (j.embedding <=> @embedding) AS cosine
FROM jobs j
WHERE j.embedding IS NOT NULL
  AND j.embedding_model = @model
  AND (CAST(@max_years AS int) IS NULL
       OR COALESCE(j.required_years, j.min_years) IS NULL
       OR COALESCE(j.required_years, j.min_years) <= CAST(@max_years AS int))
ORDER BY j.embedding <=> @embedding
LIMIT @limit`

// FindCandidateJobs lấy tối đa q.Limit job gần hồ sơ nhất theo cosine, sau khi loại job yêu cầu
// nhiều hơn q.ExpYears + ExpToleranceYears năm kinh nghiệm. Kết quả sắp xếp giảm dần theo Cosine.
//
// Đây là tầng lọc thô trên DB: dịch vụ AI vẫn chạy lại đủ bộ lọc cứng (kinh nghiệm, kỹ năng bắt buộc)
// trên tập này trước khi rerank. Lọc ở đây để Python không phải nhận và rerank job chắc chắn bị loại.
func (r *JobRepository) FindCandidateJobs(ctx context.Context, q CandidateQuery) ([]CandidateJob, error) {
	if q.EmbeddingModel == "" || len(q.Embedding.Slice()) == 0 {
		return nil, fmt.Errorf("find candidate jobs: embedding and embedding model are required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultCandidateLimit
	}
	limit = min(limit, maxCandidateLimit)
	var maxYears *int
	if q.ExpYears != nil {
		v := *q.ExpYears + ExpToleranceYears
		maxYears = &v
	}

	var out []CandidateJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// set_config(..., true) chỉ có hiệu lực trong transaction này (như SET LOCAL), không rò sang
		// kết nối khác trong pool. Không có iterative_scan, HNSW chỉ lấy ef_search ứng viên gần nhất
		// rồi mới lọc WHERE, nên có thể trả về ít hơn LIMIT dù DB còn đủ job phù hợp.
		efSearch := strconv.Itoa(max(limit, defaultHNSWEfSearch))
		if err := tx.Exec(`SELECT set_config('hnsw.iterative_scan', 'strict_order', true),
			set_config('hnsw.ef_search', ?, true)`, efSearch).Error; err != nil {
			return fmt.Errorf("configure hnsw scan: %w", err)
		}

		err := tx.Raw(candidateJobsSQL, map[string]any{
			"embedding": q.Embedding, "model": q.EmbeddingModel, "max_years": maxYears, "limit": limit,
		}).Scan(&out).Error
		if err != nil {
			return fmt.Errorf("query candidate jobs: %w", err)
		}
		if len(out) == 0 {
			return nil
		}

		// Kỹ năng của cả tập trong một truy vấn, thay vì một truy vấn cho mỗi job.
		ids := make([]uint, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		var skills []models.JobSkill
		if err := tx.Where("job_id IN ?", ids).Order("id").Find(&skills).Error; err != nil {
			return fmt.Errorf("load skills of candidate jobs: %w", err)
		}
		byJob := make(map[uint][]models.JobSkill, len(out))
		for _, s := range skills {
			byJob[s.JobID] = append(byJob[s.JobID], s)
		}
		for i := range out {
			out[i].JobSkills = byJob[out[i].ID]
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find candidate jobs: %w", err)
	}
	return out, nil
}

type JobRepository struct {
	db *gorm.DB
}

func NewJobRepository(db *gorm.DB) *JobRepository {
	return &JobRepository{db: db}
}

// EnsureSource returns the ID of the source named name, creating it with url if missing.
func (r *JobRepository) EnsureSource(ctx context.Context, name, url string) (uint, error) {
	src := models.Source{Name: name, URL: url}
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "name"}}, DoNothing: true}).
		Create(&src).Error
	if err != nil {
		return 0, fmt.Errorf("create source %q: %w", name, err)
	}
	if src.ID != 0 {
		return src.ID, nil
	}
	// DO NOTHING returns no row when the source already exists.
	if err := r.db.WithContext(ctx).Where("name = ?", name).Take(&src).Error; err != nil {
		return 0, fmt.Errorf("find source %q: %w", name, err)
	}
	return src.ID, nil
}

// ExistingUniqueIDs returns which of ids are already stored as a Job.UniqueIdentifier.
func (r *JobRepository) ExistingUniqueIDs(ctx context.Context, ids []string) (map[string]struct{}, error) {
	found := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	var existing []string
	err := r.db.WithContext(ctx).Model(&models.Job{}).
		Where("unique_identifier IN ?", ids).
		Pluck("unique_identifier", &existing).Error
	if err != nil {
		return nil, fmt.Errorf("find existing jobs: %w", err)
	}
	for _, id := range existing {
		found[id] = struct{}{}
	}
	return found, nil
}

// IDsByUniqueIDs maps each stored Job.UniqueIdentifier among uniqueIDs to its ID.
func (r *JobRepository) IDsByUniqueIDs(ctx context.Context, uniqueIDs []string) (map[string]uint, error) {
	out := make(map[string]uint, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		ID               uint
		UniqueIdentifier string
	}
	err := r.db.WithContext(ctx).Model(&models.Job{}).Select("id", "unique_identifier").
		Where("unique_identifier IN ?", uniqueIDs).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("find job ids: %w", err)
	}
	for _, row := range rows {
		out[row.UniqueIdentifier] = row.ID
	}
	return out, nil
}

// UpsertWithSkills inserts job or, when its UniqueIdentifier already exists, updates
// it in place; the job's skills are then replaced by skills. job.ID is set on return.
//
// The AI analysis columns are overwritten only when job carries an analysis
// (AnalyzedAt set), so re-importing a plain scraped job keeps an earlier analysis.
func (r *JobRepository) UpsertWithSkills(ctx context.Context, job *models.Job, skills []string) error {
	updates := []string{"source_id", "title", "company", "description", "url", "raw_salary", "status", "posted_at"}
	if job.AnalyzedAt != nil {
		updates = append(updates, "location", "is_remote", "min_years", "required_years", "languages",
			"embedding", "embedding_model", "analyzed_at")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "unique_identifier"}},
			DoUpdates: clause.AssignmentColumns(updates),
		}).Omit("JobSkills", "AIMatches", "Source").Create(job).Error
		if err != nil {
			return fmt.Errorf("upsert job %q: %w", job.UniqueIdentifier, err)
		}

		if err := tx.Where("job_id = ?", job.ID).Delete(&models.JobSkill{}).Error; err != nil {
			return fmt.Errorf("delete skills of job %d: %w", job.ID, err)
		}
		if len(skills) == 0 {
			return nil
		}
		rows := make([]models.JobSkill, len(skills))
		for i, s := range skills {
			rows[i] = models.JobSkill{JobID: job.ID, SkillName: s}
		}
		if err := tx.Create(&rows).Error; err != nil {
			return fmt.Errorf("insert skills of job %d: %w", job.ID, err)
		}
		return nil
	})
}
